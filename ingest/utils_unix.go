//go:build !windows
// +build !windows

/*************************************************************************
 * Copyright 2022 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package ingest

import (
	"os"

	"github.com/google/renameio"
)

func atomicFileWrite(pth string, data []byte, mode os.FileMode) error {
	return renameio.WriteFile(pth, data, mode)
}
