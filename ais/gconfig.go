// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"os"
	"path"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

type (
	globalConfig struct {
		cmn.Config
	}
	configOwner struct {
		sync.Mutex
		config     atomic.Pointer // pointer to globalConf
		daemonType string
	}
)

var _ revs = (*globalConfig)(nil)

func (config *globalConfig) tag() string     { return revsConfTag }
func (config *globalConfig) version() int64  { return config.Version }
func (config *globalConfig) marshal() []byte { return cmn.MustLocalMarshal(config) }
func (config *globalConfig) clone() *globalConfig {
	clone := &globalConfig{}
	cmn.MustMorphMarshal(config, clone)
	return clone
}

func (config *globalConfig) String() string {
	if config == nil {
		return "Conf <nil>"
	}
	return fmt.Sprintf("Conf v%d", config.Version)
}

////////////
// config //
////////////

func newConfOwner(daemonType string) *configOwner {
	return &configOwner{daemonType: daemonType}
}

func (co *configOwner) get() *globalConfig {
	return (*globalConfig)(co.config.Load())
}

func (co *configOwner) put(config *globalConfig) {
	cmn.Assert(config.MetaVersion != 0)
	config.SetRole(co.daemonType)
	co.config.Store(unsafe.Pointer(config))
}

func cfgBeginUpdate() *cmn.Config { return cmn.GCO.BeginUpdate() }
func cfgDiscardUpdate()           { cmn.GCO.DiscardUpdate() }
func cfgCommitUpdate(config *cmn.Config, detail string) (err error) {
	if err = jsp.SaveConfig(config); err != nil {
		cmn.GCO.DiscardUpdate()
		return fmt.Errorf("FATAL: failed writing config %s: %s, %v", cmn.GCO.GetGlobalConfigPath(), detail, err)
	}
	cmn.GCO.CommitUpdate(config)
	return
}

// Update the global config on primary proxy.
func (co *configOwner) modify(toUpdate *cmn.ConfigToUpdate, detail string) error {
	co.Lock()
	defer co.Unlock()
	config := co.get().clone()
	err := jsp.SetConfigInMem(toUpdate, &config.Config)
	if err != nil {
		return err
	}
	config.Version++
	config.LastUpdated = time.Now().String()
	if err = co.persist(config); err != nil {
		return fmt.Errorf("FATAL: failed persist config for %q, err: %v", detail, err)
	}
	co.updateGCO()
	return nil
}

func (co *configOwner) persist(config *globalConfig) error {
	co.put(config)
	local := cmn.GCO.GetLocal()
	savePath := path.Join(local.ConfigDir, gconfFname)
	if err := jsp.Save(savePath, config, jsp.PlainLocal()); err != nil {
		return err
	}
	return nil
}

func (co *configOwner) updateGCO() (err error) {
	cmn.GCO.BeginUpdate()
	config := co.get().clone()
	config.SetLocalConf(cmn.GCO.GetLocal())
	if err = config.Validate(); err != nil {
		return
	}
	cmn.GCO.CommitUpdate(&config.Config)
	return
}

func (co *configOwner) load() (err error) {
	localConf := cmn.GCO.GetLocal()
	config := &globalConfig{}
	_, err = jsp.Load(path.Join(localConf.ConfigDir, gconfFname), config, jsp.Plain())
	if err == nil {
		if config.MetaVersion == 0 {
			config.MetaVersion = cmn.MetaVersion
		}
		co.put(config)
		return co.updateGCO()
	}
	if !os.IsNotExist(err) {
		return
	}
	// If gconf file is missing, assume conf provided through CLI as global.
	// NOTE: We cannot use GCO.Get() here as cmn.GCO may also contain custom config.
	config = &globalConfig{}
	_, err = jsp.Load(cmn.GCO.GetGlobalConfigPath(), config, jsp.Plain())
	if err != nil {
		return
	}
	config.MetaVersion = cmn.MetaVersion
	co.put(config)
	return
}

func setConfig(toUpdate *cmn.ConfigToUpdate, transient bool) error {
	config := cfgBeginUpdate()
	err := jsp.SetConfigInMem(toUpdate, config)
	if transient || err != nil {
		cmn.GCO.DiscardUpdate()
		return err
	}
	_ = transient // Ignore transient for now
	cfgCommitUpdate(config, "set config")
	return nil
}