//go:build !cgo

package main

import (
	"encoding/json"
	"fmt"
)

func callHost(method string, payload []byte) (json.RawMessage, error) {
	_ = payload
	return nil, fmt.Errorf("host callback %s unavailable without cgo", method)
}
