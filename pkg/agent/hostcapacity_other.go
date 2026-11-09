//go:build !darwin && !linux

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import "errors"

// errNoSysctl is what the capacity reader returns off macOS and Linux, where
// sysctl names it asks for do not exist.
var errNoSysctl = errors.New("host capacity is only read on macOS and Linux")

// DetectHostCapacity reports nothing off macOS and Linux. It goes through the
// same reader as darwin, with every read failing, so the parsing helpers are
// compiled and used on every OS.
func DetectHostCapacity() HostCapacity {
	return hostCapacityFromSysctl(func(string) (string, error) {
		return "", errNoSysctl
	})
}
