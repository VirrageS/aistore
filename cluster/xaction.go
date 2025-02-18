// Package cluster provides local access to cluster-level metadata
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package cluster

import (
	"sync"
	"time"
)

type QuiRes int

const (
	QuiInactiveCB = QuiRes(iota) // e.g., no pending requests (NOTE: used exclusively by `quicb` callbacks)
	QuiActive                    // active (e.g., receiving data)
	QuiActiveRet                 // active that immediately breaks waiting for quiecscence
	QuiDone                      // all done
	QuiAborted                   // aborted
	QuiTimeout                   // timeout
	Quiescent                    // idle => quiescent
)

type (
	QuiCB func(elapsed time.Duration) QuiRes // see enum below

	Xact interface {
		Run(*sync.WaitGroup)
		ID() string
		Kind() string
		Bck() *Bck
		FromTo() (*Bck, *Bck)
		StartTime() time.Time
		EndTime() time.Time
		Finished() bool
		Running() bool
		IsAborted() bool
		AbortErr() error
		AbortedAfter(time.Duration) error
		ChanAbort() <-chan error
		Quiesce(time.Duration, QuiCB) QuiRes
		Result() (any, error)
		Snap() XactSnap

		// reporting: log, err
		String() string
		Name() string

		// modifiers
		Finish(error)
		Abort(error) bool
		AddNotif(n Notif)

		// common stats
		Objs() int64
		ObjsAdd(int, int64)    // locally processed
		OutObjsAdd(int, int64) // transmit
		InObjsAdd(int, int64)  // receive
		InBytes() int64
		OutBytes() int64
	}

	XactSnap interface {
		IsAborted() bool
		Running() bool
		Finished() bool
	}
)
