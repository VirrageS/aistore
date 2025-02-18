// Package cluster provides common interfaces and local access to cluster-level metadata
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package cluster

type (
	RemAis struct {
		URL   string `json:"url"`
		Alias string `json:"alias"`
		UUID  string `json:"uuid"` // Smap.UUID
		Smap  *Smap  `json:"smap"`
	}
	Remotes struct {
		A   []*RemAis `json:"a"`
		Ver int64     `json:"ver"`
	}
)
