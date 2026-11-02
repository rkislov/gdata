//go:build !linux

// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package removable

import "errors"

func checkMount(Config) error {
	return errors.New("removable filesystem UUID validation requires Linux")
}
func syncVolume(Config) error {
	return errors.New("removable filesystem synchronization requires Linux")
}

func anchorVolume(c Config) (string, func(), error) {
	return "", func() {}, errors.New("removable storage requires Linux")
}
