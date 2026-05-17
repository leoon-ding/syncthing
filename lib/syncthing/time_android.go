// Copyright (C) 2021 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package syncthing

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var androidBuildPropPaths = []string{
	"/system/build.prop",
	"/system/system/build.prop",
	"/vendor/build.prop",
	"/product/build.prop",
}

// https://github.com/golang/go/issues/20455#issuecomment-342287698
func init() {
	// Android 11 and earlier may kill c-shared processes with SIGSYS when
	// os/exec triggers Go's pidfd probe. When the SDK level can't be determined,
	// prefer the historical UTC fallback over guessing and risking os/exec on an
	// older device.
	sdk, ok := androidSDKLevel()
	if !ok || sdk < 31 {
		time.Local = time.UTC
		return
	}

	out, err := exec.Command("/system/bin/getprop", "persist.sys.timezone").Output()
	if err != nil {
		return
	}
	z, err := time.LoadLocation(strings.TrimSpace(string(out)))
	if err != nil {
		return
	}
	time.Local = z
}

func androidSDKLevel() (int, bool) {
	for _, path := range androidBuildPropPaths {
		value, ok := readBuildPropValue(path, "ro.build.version.sdk")
		if !ok {
			continue
		}
		level, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		return level, true
	}
	return 0, false
}

func readBuildPropValue(path, key string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	prefix := key + "="
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
		}
	}

	return "", false
}
