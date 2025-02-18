// Package sys provides methods to read system information
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package sys

import (
	"fmt"
	"math"

	"github.com/NVIDIA/aistore/cmn/cos"
)

// Memory stats for the host OS or for container, depending on where the app is running.
// For the host, returns an error if memory cannot be read. If the function fails to read
// container's stats, it returns host memory. Swap stats, however, are _always_ host stats.

type MemStat struct {
	Total      uint64
	Used       uint64
	Free       uint64
	BuffCache  uint64
	ActualFree uint64
	ActualUsed uint64
	SwapTotal  uint64
	SwapFree   uint64
	SwapUsed   uint64
}

var mem0 = MemStat{ActualFree: math.MaxUint64}

func (mem *MemStat) Get() error {
	if !containerized {
		return mem.host()
	}
	return mem.container()
}

func (mem *MemStat) String() string {
	var (
		used      = cos.B2S(int64(mem.Used), 0)
		free      = cos.B2S(int64(mem.Free), 0)
		buffcache = cos.B2S(int64(mem.BuffCache), 0)
		actfree   = cos.B2S(int64(mem.ActualFree), 0)
	)
	return fmt.Sprintf("used %s, free %s, buffcache %s, actfree %s", used, free, buffcache, actfree)
}
