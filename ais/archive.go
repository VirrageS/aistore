// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// References:
// * https://en.wikipedia.org/wiki/List_of_file_signatures
// * https://www.iana.org/assignments/media-types/media-types.xhtml#application

const (
	sizeDetectMime = 512
)

const (
	unknownMime = "unknown mime type"
)

type (
	cslLimited struct {
		io.LimitedReader
	}
	cslClose struct {
		gzr io.ReadCloser
		R   io.Reader
		N   int64
	}
	cslFile struct {
		file io.ReadCloser
		size int64
	}

	detect struct {
		offset int
		sig    []byte
		mime   string // '.' + IANA mime
	}
)

var (
	magicTar  = detect{offset: 257, sig: []byte("ustar"), mime: cos.ExtTar}
	magicGzip = detect{sig: []byte{0x1f, 0x8b}, mime: cos.ExtTarTgz}
	magicZip  = detect{sig: []byte{0x50, 0x4b}, mime: cos.ExtZip}

	allMagics = []detect{magicTar, magicGzip, magicZip} // NOTE: must contain all
)

func (csl *cslLimited) Size() int64  { return csl.N }
func (csl *cslLimited) Close() error { return nil }

func (csc *cslClose) Read(b []byte) (int, error) { return csc.R.Read(b) }
func (csc *cslClose) Size() int64                { return csc.N }
func (csc *cslClose) Close() error               { return csc.gzr.Close() }

func (csf *cslFile) Read(b []byte) (int, error) { return csf.file.Read(b) }
func (csf *cslFile) Size() int64                { return csf.size }
func (csf *cslFile) Close() error               { return csf.file.Close() }

func notFoundInArch(filename, archname string) error {
	return cmn.NewNotFoundError("file %q in archive %q", filename, archname)
}

/////////////////////////
// GET OBJECT: archive //
/////////////////////////

func (goi *getObjInfo) freadArch(file *os.File) (cos.ReadCloseSizer, error) {
	mime, err := goi.mime(file)
	if err != nil {
		return nil, err
	}
	// NOTE: not supporting `--absolute-names`
	filename, archname := goi.archive.filename, filepath.Join(goi.lom.Bucket().Name, goi.lom.ObjName)
	if goi.archive.filename[0] == filepath.Separator {
		filename = goi.archive.filename[1:]
	}
	switch mime {
	case cos.ExtTar:
		return freadTar(file, filename, archname)
	case cos.ExtTarTgz, cos.ExtTgz:
		return freadTgz(file, filename, archname)
	case cos.ExtZip:
		return freadZip(file, filename, archname, goi.lom.Size())
	default:
		debug.Assert(false)
		return nil, errors.New(unknownMime)
	}
}

func (goi *getObjInfo) mime(file *os.File) (m string, err error) {
	objname := goi.lom.ObjName
	switch {
	case strings.HasSuffix(objname, cos.ExtTar):
		return cos.ExtTar, nil
	case strings.HasSuffix(objname, cos.ExtTarTgz):
		return cos.ExtTarTgz, nil
	case strings.HasSuffix(objname, cos.ExtTgz):
		return cos.ExtTgz, nil
	case strings.HasSuffix(objname, cos.ExtZip):
		return cos.ExtZip, nil
	}
	// simplified auto-detection
	var (
		buf, slab = goi.t.smm.Alloc(sizeDetectMime)
		n         int
	)
	n, err = file.Read(buf)
	for _, magic := range allMagics {
		if n > magic.offset && bytes.HasPrefix(buf[magic.offset:], magic.sig) {
			m = magic.mime
			break
		}
	}
	if m == "" {
		if err == nil {
			err = errors.New(unknownMime)
		} else {
			err = fmt.Errorf("%s (%v)", unknownMime, err)
		}
	} else {
		err = nil
	}
	if n > 0 {
		file.Seek(0, io.SeekStart)
	}
	slab.Free(buf)
	return
}

func freadTar(reader io.Reader, filename, archname string) (*cslLimited, error) {
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				err = notFoundInArch(filename, archname)
			}
			return nil, err
		}
		if hdr.Name != filename {
			continue
		}
		return &cslLimited{LimitedReader: io.LimitedReader{R: reader, N: hdr.Size}}, nil
	}
}

func freadTgz(reader io.Reader, filename, archname string) (csc *cslClose, err error) {
	var (
		gzr *gzip.Reader
		csl *cslLimited
	)
	if gzr, err = gzip.NewReader(reader); err != nil {
		return
	}
	if csl, err = freadTar(gzr, filename, archname); err != nil {
		return
	}
	csc = &cslClose{gzr: gzr /*to close*/, R: csl /*to read from*/, N: csl.N /*size*/}
	return
}

func freadZip(readerAt cos.ReadReaderAt, filename, archname string, size int64) (csf *cslFile, err error) {
	var zr *zip.Reader
	if zr, err = zip.NewReader(readerAt, size); err != nil {
		return
	}
	for _, f := range zr.File {
		header := f.FileHeader
		if header.Name != filename {
			continue
		}
		finfo := f.FileInfo()
		if finfo.IsDir() {
			continue
		}
		csf = &cslFile{size: finfo.Size()}
		csf.file, err = f.Open()
		return
	}
	err = notFoundInArch(filename, archname)
	return
}
