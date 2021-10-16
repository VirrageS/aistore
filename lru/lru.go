// Package lru provides least recently used cache replacement policy for stored objects
// and serves as a generic garbage-collection mechanism for orphaned workfiles.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package lru

import (
	"container/heap"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xreg"
)

// The LRU module implements a well-known least-recently-used cache replacement policy.
//
// LRU-driven eviction is based on the two configurable watermarks: config.LRU.LowWM and
// config.LRU.HighWM (section "lru" in the /deploy/dev/local/aisnode_config.sh).
// When and if exceeded, AIStore target will start gradually evicting objects from its
// stable storage: oldest first access-time wise.
//
// LRU is implemented as a so-called extended action (aka x-action, see xaction.go) that gets
// triggered when/if a used local capacity exceeds high watermark (config.LRU.HighWM). LRU then
// runs automatically. In order to reduce its impact on the live workload, LRU throttles itself
// in accordance with the current storage-target's utilization (see xaction_throttle.go).
//
// There's only one API that this module provides to the rest of the code:
//   - runLRU - to initiate a new LRU extended action on the local target
// All other methods are private to this module and are used only internally.

// TODO: extend LRU to remove CTs beyond just []string{fs.WorkfileType, fs.ObjectType}

// LRU defaults/tunables
const (
	minEvictThresh = 10 * cos.MiB
	capCheckThresh = 256 * cos.MiB // capacity checking threshold, when exceeded may result in lru throttling
)

type (
	InitLRU struct {
		T                   cluster.Target
		Xaction             *Xaction
		StatsT              stats.Tracker
		Buckets             []cmn.Bck // list of buckets to run LRU
		GetFSUsedPercentage func(path string) (usedPercentage int64, ok bool)
		GetFSStats          func(path string) (blocks, bavail uint64, bsize int64, err error)
		Force               bool // Ignore LRU prop when set to be true.
		Cleanup             bool // True - only remove trash, False - run LRU
	}

	// minHeap keeps fileInfo sorted by access time with oldest
	// on top of the heap.
	minHeap []*cluster.LOM

	// parent - contains mpath joggers
	lruP struct {
		wg      sync.WaitGroup
		joggers map[string]*lruJ
		ini     InitLRU
	}

	// lruJ represents a single LRU context and a single /jogger/
	// that traverses and evicts a single given mountpath.
	lruJ struct {
		// runtime
		curSize   int64
		totalSize int64 // difference between lowWM size and used size
		newest    int64
		heap      *minHeap
		oldWork   []string
		misplaced struct {
			loms []*cluster.LOM
			ec   []*cluster.CT // EC slices and replicas without corresponding metafiles (CT FQN -> Meta FQN)
		}
		bck cmn.Bck
		now int64
		// init-time
		p         *lruP
		ini       *InitLRU
		stopCh    chan struct{}
		joggers   map[string]*lruJ
		mpathInfo *fs.MountpathInfo
		config    *cmn.Config
		// runtime
		throttle    bool
		allowDelObj bool
	}

	Factory struct {
		xreg.RenewBase
		xact *Xaction
	}
	Xaction struct {
		xaction.XactBase
	}
)

// interface guard
var (
	_ xreg.Renewable = (*Factory)(nil)
)

func init() { xreg.RegNonBckXact(&Factory{}) }

func rmMisplaced() bool {
	g, l := xreg.GetRebMarked(), xreg.GetResilverMarked()
	return !g.Interrupted && !l.Interrupted && g.Xact == nil && l.Xact == nil
}

/////////////
// Factory //
/////////////

func (*Factory) New(args xreg.Args, _ *cluster.Bck) xreg.Renewable {
	return &Factory{RenewBase: xreg.RenewBase{Args: args}}
}

func (p *Factory) Start() error {
	p.xact = &Xaction{}
	p.xact.InitBase(p.UUID(), cmn.ActLRU, nil)
	return nil
}

func (*Factory) Kind() string        { return cmn.ActLRU }
func (p *Factory) Get() cluster.Xact { return p.xact }

func (p *Factory) WhenPrevIsRunning(prevEntry xreg.Renewable) (wpr xreg.WPR, err error) {
	err = fmt.Errorf("%s is already running - not starting %q", prevEntry.Get(), p.Str(p.Kind()))
	return
}

func Run(ini *InitLRU) {
	var (
		xlru              = ini.Xaction
		config            = cmn.GCO.Get()
		availablePaths, _ = fs.Get()
		num               = len(availablePaths)
		joggers           = make(map[string]*lruJ, num)
		parent            = &lruP{joggers: joggers, ini: *ini}
	)
	glog.Infof("[lru] %s started: dont-evict-time %v", xlru, config.LRU.DontEvictTime)
	if num == 0 {
		glog.Warning(fs.ErrNoMountpaths)
		xlru.Finish(fs.ErrNoMountpaths)
		return
	}
	for mpath, mpathInfo := range availablePaths {
		h := make(minHeap, 0, 64)
		joggers[mpath] = &lruJ{
			heap:      &h,
			oldWork:   make([]string, 0, 64),
			stopCh:    make(chan struct{}, 1),
			mpathInfo: mpathInfo,
			config:    config,
			ini:       &parent.ini,
			p:         parent,
		}
		joggers[mpath].misplaced.loms = make([]*cluster.LOM, 0, 64)
		joggers[mpath].misplaced.ec = make([]*cluster.CT, 0, 64)
	}
	providers := cmn.Providers.ToSlice()

	for _, j := range joggers {
		parent.wg.Add(1)
		j.joggers = joggers
		go j.run(providers)
	}
	parent.wg.Wait()

	for _, j := range joggers {
		j.stop()
	}
	xlru.Finish(nil)
}

func (*Xaction) Run(*sync.WaitGroup) { debug.Assert(false) }

//////////////////////
// mountpath jogger //
//////////////////////

func (j *lruJ) String() string {
	return fmt.Sprintf("%s: (%s, %s)", j.ini.T.Snode(), j.ini.Xaction, j.mpathInfo)
}

func (j *lruJ) stop() { j.stopCh <- struct{}{} }

func (j *lruJ) run(providers []string) {
	var err error
	defer j.p.wg.Done()
	if err = j.removeTrash(); err != nil {
		goto ex
	}
	if !j.p.ini.Cleanup {
		// compute the size (bytes) to free up (and do it after removing the $trash)
		if err = j.evictSize(); err != nil {
			goto ex
		}
		if j.totalSize < minEvictThresh {
			glog.Infof("[lru] %s: used cap below threshold, nothing to do", j)
			return
		}
	}
	if len(j.ini.Buckets) != 0 {
		glog.Infof("[lru] %s: freeing-up %s", j, cos.B2S(j.totalSize, 2))
		err = j.jogBcks(j.ini.Buckets, j.ini.Force)
	} else {
		err = j.jog(providers)
	}
ex:
	if err == nil || cmn.IsErrBucketNought(err) || cmn.IsErrObjNought(err) {
		return
	}
	glog.Errorf("[lru] %s: exited with err %v", j, err)
}

func (j *lruJ) jog(providers []string) (err error) {
	glog.Infof("%s: freeing-up %s", j, cos.B2S(j.totalSize, 2))
	for _, provider := range providers { // for each provider (NOTE: ordering is random)
		var (
			bcks []cmn.Bck
			opts = fs.Options{
				Mpath: j.mpathInfo,
				Bck:   cmn.Bck{Provider: provider, Ns: cmn.NsGlobal},
			}
		)
		if bcks, err = fs.AllMpathBcks(&opts); err != nil {
			return
		}
		if err = j.jogBcks(bcks, false); err != nil {
			return
		}
	}
	return
}

func (j *lruJ) jogBcks(bcks []cmn.Bck, force bool) (err error) {
	if len(bcks) == 0 {
		return
	}
	if len(bcks) > 1 {
		j.sortBsize(bcks)
	}
	for _, bck := range bcks { // for each bucket under a given provider
		var size int64
		j.bck = bck
		if j.allowDelObj, err = j.allow(); err != nil {
			if cmn.IsErrBckNotFound(err) || cmn.IsErrRemoteBckNotFound(err) {
				j.ini.T.TrashNonExistingBucket(bck)
			} else {
				// TODO: config option to scrub `fs.AllMpathBcks` buckets
				glog.Errorf("%s: %v - skipping %s", j, err, bck)
			}
			err = nil
			continue
		}
		j.allowDelObj = j.allowDelObj || force
		if size, err = j.jogBck(); err != nil {
			return
		}
		if !j.p.ini.Cleanup {
			if size < cos.KiB {
				continue
			}
			// recompute size-to-evict
			if err = j.evictSize(); err != nil {
				return
			}
			if j.totalSize < cos.KiB {
				return
			}
		}
	}
	return
}

func (j *lruJ) removeTrash() (err error) {
	trashDir := j.mpathInfo.MakePathTrash()
	err = fs.Scanner(trashDir, func(fqn string, de fs.DirEntry) error {
		if de.IsDir() {
			if err := os.RemoveAll(fqn); err == nil {
				usedPct, ok := j.ini.GetFSUsedPercentage(j.mpathInfo.Path)
				if ok && usedPct < j.config.LRU.HighWM {
					if err := j._throttle(usedPct); err != nil {
						return err
					}
				}
			} else {
				glog.Errorf("%s: %v", j, err)
			}
		} else if err := os.Remove(fqn); err != nil {
			glog.Errorf("%s: %v", j, err)
		}
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		err = nil
	}
	return
}

func (j *lruJ) jogBck() (size int64, err error) {
	// 1. init per-bucket min-heap (and reuse the slice)
	h := (*j.heap)[:0]
	j.heap = &h
	heap.Init(j.heap)

	// 2. collect
	// TODO: LRU other CTs besides WorkfileType
	opts := &fs.Options{
		Mpath:    j.mpathInfo,
		Bck:      j.bck,
		CTs:      []string{fs.WorkfileType, fs.ObjectType, fs.ECSliceType, fs.ECMetaType},
		Callback: j.walk,
		Sorted:   false,
	}
	j.now = time.Now().UnixNano()
	if err = fs.Walk(opts); err != nil {
		return
	}
	// 3. evict
	size, err = j.evict()
	return
}

func (j *lruJ) visitCT(parsedFQN fs.ParsedFQN, fqn string) {
	switch parsedFQN.ContentType {
	case fs.WorkfileType:
		_, base := filepath.Split(fqn)
		contentResolver := fs.CSM.RegisteredContentTypes[fs.WorkfileType]
		_, old, ok := contentResolver.ParseUniqueFQN(base)
		// workfiles: remove old or do nothing
		if ok && old {
			j.oldWork = append(j.oldWork, fqn)
		}
	case fs.ECSliceType:
		// EC slices:
		// - EC enabled: remove only slices with missing metafiles
		// - EC disabled: remove all slices
		ct, err := cluster.NewCTFromFQN(fqn, j.p.ini.T.Bowner())
		if err != nil || !ct.Bck().Props.EC.Enabled {
			j.oldWork = append(j.oldWork, fqn)
			return
		}
		if err := ct.LoadFromFS(); err != nil {
			return
		}
		// Saving a CT is not atomic: first it saves CT, then its metafile
		// follows. Ignore just updated CTs to avoid processing incomplete data.
		if ct.MtimeUnix()+int64(j.config.LRU.DontEvictTime) > j.now {
			return
		}
		metaFQN := fs.CSM.GenContentFQN(ct, fs.ECMetaType, "")
		if fs.Access(metaFQN) != nil {
			j.misplaced.ec = append(j.misplaced.ec, ct)
		}
	case fs.ECMetaType:
		// EC metafiles:
		// - EC enabled: remove only without corresponding slice or replica
		// - EC disabled: remove all metafiles
		ct, err := cluster.NewCTFromFQN(fqn, j.p.ini.T.Bowner())
		if err != nil || !ct.Bck().Props.EC.Enabled {
			j.oldWork = append(j.oldWork, fqn)
			return
		}
		// Metafile is saved the last. If there is no corresponding replica or
		// slice, it is safe to remove the stray metafile.
		sliceCT := ct.Clone(fs.ECSliceType)
		if fs.Access(sliceCT.FQN()) == nil {
			return
		}
		objCT := ct.Clone(fs.ObjectType)
		if fs.Access(objCT.FQN()) == nil {
			return
		}
		j.oldWork = append(j.oldWork, fqn)
	default:
		debug.Assertf(false, "Unsupported content type: %s", parsedFQN.ContentType)
	}
}

func (j *lruJ) visitLOM(parsedFQN fs.ParsedFQN) {
	if !j.allowDelObj {
		return
	}
	lom := &cluster.LOM{ObjName: parsedFQN.ObjName}
	err := lom.Init(j.bck)
	if err != nil {
		return
	}
	err = lom.Load(false /*cache it*/, false /*locked*/)
	if err != nil {
		return
	}
	if lom.AtimeUnix()+int64(j.config.LRU.DontEvictTime) > j.now {
		return
	}
	if lom.HasCopies() && lom.IsCopy() {
		return
	}

	if !lom.IsHRW() {
		if lom.Bprops().EC.Enabled {
			metaFQN := fs.CSM.GenContentFQN(lom, fs.ECMetaType, "")
			if fs.Access(metaFQN) != nil {
				j.misplaced.ec = append(j.misplaced.ec, cluster.NewCTFromLOM(lom, fs.ObjectType))
			}
		} else {
			j.misplaced.loms = append(j.misplaced.loms, lom)
		}
		return
	}

	if j.p.ini.Cleanup {
		return
	}

	// do nothing if the heap's curSize >= totalSize and
	// the file is more recent then the the heap's newest.
	if j.curSize >= j.totalSize && lom.AtimeUnix() > j.newest {
		return
	}
	heap.Push(j.heap, lom)
	j.curSize += lom.SizeBytes()
	if lom.AtimeUnix() > j.newest {
		j.newest = lom.AtimeUnix()
	}
}

func (j *lruJ) walk(fqn string, de fs.DirEntry) error {
	if de.IsDir() {
		return nil
	}
	if err := j.yieldTerm(); err != nil {
		return err
	}
	parsedFQN, _, err := cluster.ResolveFQN(fqn)
	if err != nil {
		return nil
	}
	if parsedFQN.ContentType != fs.ObjectType {
		j.visitCT(parsedFQN, fqn)
	} else {
		j.visitLOM(parsedFQN)
	}

	return nil
}

func (j *lruJ) evict() (size int64, err error) {
	var (
		fevicted, bevicted int64
		capCheck           int64
		h                  = j.heap
		xlru               = j.ini.Xaction
	)
	// 1. rm older work
	for _, workfqn := range j.oldWork {
		finfo, erw := os.Stat(workfqn)
		if erw == nil {
			if err := cos.RemoveFile(workfqn); err != nil {
				glog.Warningf("Failed to remove old work %q: %v", workfqn, err)
			} else {
				size += finfo.Size()
			}
		}
	}
	j.oldWork = j.oldWork[:0]

	// 2. rm misplaced
	if rmMisplaced() {
		for _, lom := range j.misplaced.loms {
			var (
				fqn     = lom.FQN
				removed bool
			)
			lom = &cluster.LOM{ObjName: lom.ObjName} // yes placed
			if lom.Init(j.bck) != nil {
				removed = os.Remove(fqn) == nil
			} else if lom.FromFS() != nil {
				removed = os.Remove(fqn) == nil
			} else {
				removed, _ = lom.DelExtraCopies(fqn)
			}
			if !removed && lom.FQN != fqn {
				removed = os.Remove(fqn) == nil
			}
			if removed {
				if capCheck, err = j.postRemove(capCheck, lom.SizeBytes(true /*not loaded*/)); err != nil {
					return
				}
			}
		}
	}
	j.misplaced.loms = j.misplaced.loms[:0]

	// 3. rm EC slices and replicas that are still without correcponding metafile
	for _, ct := range j.misplaced.ec {
		metaFQN := fs.CSM.GenContentFQN(ct, fs.ECMetaType, "")
		if fs.Access(metaFQN) == nil {
			continue
		}
		if os.Remove(ct.FQN()) == nil {
			if capCheck, err = j.postRemove(capCheck, ct.SizeBytes()); err != nil {
				return
			}
		}
	}
	j.misplaced.ec = j.misplaced.ec[:0]

	// 4. evict(sic!) and house-keep
	for h.Len() > 0 && j.totalSize > 0 {
		lom := heap.Pop(h).(*cluster.LOM)
		if !evictObj(lom) {
			continue
		}
		objSize := lom.SizeBytes(true /*not loaded*/)
		bevicted += objSize
		size += objSize
		fevicted++
		if capCheck, err = j.postRemove(capCheck, objSize); err != nil {
			return
		}
	}
	j.ini.StatsT.Add(stats.LruEvictSize, bevicted)
	j.ini.StatsT.Add(stats.LruEvictCount, fevicted)
	xlru.ObjectsAdd(fevicted)
	xlru.BytesAdd(bevicted)
	return
}

func (j *lruJ) postRemove(prev, size int64) (capCheck int64, err error) {
	j.totalSize -= size
	capCheck = prev + size
	if err = j.yieldTerm(); err != nil {
		return
	}
	if capCheck < capCheckThresh {
		return
	}
	// init, recompute, and throttle - once per capCheckThresh
	capCheck = 0
	j.throttle = false
	j.allowDelObj, _ = j.allow()
	j.config = cmn.GCO.Get()
	j.now = time.Now().UnixNano()
	usedPct, ok := j.ini.GetFSUsedPercentage(j.mpathInfo.Path)
	if ok && usedPct < j.config.LRU.HighWM {
		err = j._throttle(usedPct)
	}
	return
}

func (j *lruJ) _throttle(usedPct int64) (err error) {
	if j.mpathInfo.IsIdle(j.config) {
		return
	}
	// throttle self
	ratioCapacity := cos.Ratio(j.config.LRU.HighWM, j.config.LRU.LowWM, usedPct)
	curr := fs.GetMpathUtil(j.mpathInfo.Path)
	ratioUtilization := cos.Ratio(j.config.Disk.DiskUtilHighWM, j.config.Disk.DiskUtilLowWM, curr)
	if ratioUtilization > ratioCapacity {
		if usedPct < (j.config.LRU.LowWM+j.config.LRU.HighWM)/2 {
			j.throttle = true
		}
		time.Sleep(cmn.ThrottleMax)
		err = j.yieldTerm()
	}
	return
}

// remove local copies that "belong" to different LRU joggers; hence, space accounting may be temporarily not precise
func evictObj(lom *cluster.LOM) (ok bool) {
	lom.Lock(true)
	if err := lom.Remove(); err == nil {
		ok = true
	} else {
		glog.Errorf("%s: failed to remove, err: %v", lom, err)
	}
	lom.Unlock(true)
	return
}

func (j *lruJ) evictSize() (err error) {
	lwm, hwm := j.config.LRU.LowWM, j.config.LRU.HighWM
	blocks, bavail, bsize, err := j.ini.GetFSStats(j.mpathInfo.Path)
	if err != nil {
		return err
	}
	used := blocks - bavail
	usedPct := used * 100 / blocks
	if usedPct < uint64(hwm) {
		return
	}
	lwmBlocks := blocks * uint64(lwm) / 100
	j.totalSize = int64(used-lwmBlocks) * bsize
	return
}

func (j *lruJ) yieldTerm() error {
	xlru := j.ini.Xaction
	select {
	case <-xlru.ChanAbort():
		return cmn.NewErrAborted(xlru.Name(), "", nil)
	case <-j.stopCh:
		return cmn.NewErrAborted(xlru.Name(), "", nil)
	default:
		if j.throttle {
			time.Sleep(cmn.ThrottleMin)
		}
		break
	}
	if xlru.Finished() {
		return cmn.NewErrAborted(xlru.Name(), "", nil)
	}
	return nil
}

// sort buckets by size
func (j *lruJ) sortBsize(bcks []cmn.Bck) {
	sized := make([]struct {
		b cmn.Bck
		v uint64
	}, len(bcks))
	for i := range bcks {
		path := j.mpathInfo.MakePathCT(bcks[i], fs.ObjectType)
		sized[i].b = bcks[i]
		sized[i].v, _ = ios.GetDirSize(path)
	}
	sort.Slice(sized, func(i, j int) bool {
		return sized[i].v > sized[j].v
	})
	for i := range bcks {
		bcks[i] = sized[i].b
	}
}

func (j *lruJ) allow() (ok bool, err error) {
	var (
		bowner = j.ini.T.Bowner()
		b      = cluster.NewBckEmbed(j.bck)
	)
	if err = b.Init(bowner); err != nil {
		return
	}
	ok = b.Props.LRU.Enabled && b.Allow(cmn.AccessObjDELETE) == nil
	return
}

//////////////
// min-heap //
//////////////

func (h minHeap) Len() int            { return len(h) }
func (h minHeap) Less(i, j int) bool  { return h[i].Atime().Before(h[j].Atime()) }
func (h minHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x interface{}) { *h = append(*h, x.(*cluster.LOM)) }
func (h *minHeap) Pop() interface{} {
	old := *h
	n := len(old)
	fi := old[n-1]
	*h = old[0 : n-1]
	return fi
}
