// Copyright 2024 The Outline Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configurl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/split"
)

func registerSplitStreamDialer(r TypeRegistry[transport.StreamDialer], typeID string, newSD BuildFunc[transport.StreamDialer]) {
	r.RegisterType(typeID, func(ctx context.Context, config *Config) (transport.StreamDialer, error) {
		sd, err := newSD(ctx, config.BaseConfig)
		if err != nil {
			return nil, err
		}
		prefixBytes, repeatsNumber, skipBytes, err := parseURL(config.URL.Opaque)
		if err != nil {
			return nil, err
		}
		return split.NewStreamDialer(sd, prefixBytes, repeatsNumber, skipBytes)
	})
}

func parseURL(splitUrl string) (prefixBytes int64, repeatsNumber int64, skipBytes int64, err error) {
	// Split the input string by commas
	parts := strings.Split(splitUrl, ",")

	// Convert all parts to integers
	values := make([]int64, len(parts))
	for i, part := range parts {
		values[i], err = strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, 0, 0, err // Return immediately if any conversion fails
		}
		if values[i] < 0 {
			return 0, 0, 0, fmt.Errorf("All numbers in split have to be positive, got %d", values[i])
		}
	}

	if len(values) > 0 {
		prefixBytes = values[0]
	}
	if len(values) > 1 {
		repeatsNumber = values[1]
	}
	if len(values) > 2 {
		skipBytes = values[2]
	}
	if len(values) > 3 {
		return 0, 0, 0, fmt.Errorf("Got too many values to parse, expected split:<number>,[<number>,<number>]. Got %s", splitUrl)
	}

	if repeatsNumber > 0 && skipBytes == 0 {
		return 0, 0, 0, fmt.Errorf(
			"If repeatsNumber is >0, then skipBytes has to be >0. Got prefixBytes=%d repeatsNumber=%d skipBytes=%d",
			prefixBytes, repeatsNumber, skipBytes)
	}

	return prefixBytes, repeatsNumber, skipBytes, nil
}
