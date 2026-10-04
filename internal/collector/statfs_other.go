//go:build !linux

package collector

import (
	"errors"

	"fncluster/internal/cluster"
)

func filesystem(string) (cluster.Filesystem, string, error) {
	return cluster.Filesystem{}, "", errors.New("statfs requires Linux")
}
