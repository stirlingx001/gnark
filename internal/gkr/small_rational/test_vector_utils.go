// Copyright 2020-2025 Consensys Software Inc.
// Licensed under the Apache License, Version 2.0. See the LICENSE file for details.

// Code generated by gnark DO NOT EDIT

package gkr

import (
	"fmt"
	"hash"

	"github.com/consensys/gnark/internal/small_rational"
	"github.com/consensys/gnark/internal/small_rational/polynomial"

	"github.com/consensys/gnark/internal/gkr/gkrtesting"
)

func toElement(i int64) *small_rational.SmallRational {
	var res small_rational.SmallRational
	res.SetInt64(i)
	return &res
}

func hashFromDescription(d gkrtesting.HashDescription) (hash.Hash, error) {
	if _type, ok := d["type"]; ok {
		switch _type {
		case "const":
			startState := int64(d["val"].(float64))
			return &messageCounter{startState: startState, step: 0, state: startState}, nil
		default:
			return nil, fmt.Errorf("unknown fake hash type \"%s\"", _type)
		}
	}
	return nil, fmt.Errorf("hash description missing type")
}

type messageCounter struct {
	startState int64
	state      int64
	step       int64
}

func (m *messageCounter) Write(p []byte) (n int, err error) {
	inputBlockSize := (len(p)-1)/small_rational.Bytes + 1
	m.state += int64(inputBlockSize) * m.step
	return len(p), nil
}

func (m *messageCounter) Sum(b []byte) []byte {
	inputBlockSize := (len(b)-1)/small_rational.Bytes + 1
	resI := m.state + int64(inputBlockSize)*m.step
	var res small_rational.SmallRational
	res.SetInt64(int64(resI))
	resBytes := res.Bytes()
	return resBytes[:]
}

func (m *messageCounter) Reset() {
	m.state = m.startState
}

func (m *messageCounter) Size() int {
	return small_rational.Bytes
}

func (m *messageCounter) BlockSize() int {
	return small_rational.Bytes
}

func newMessageCounter(startState, step int) hash.Hash {
	transcript := &messageCounter{startState: int64(startState), state: int64(startState), step: int64(step)}
	return transcript
}

func newMessageCounterGenerator(startState, step int) func() hash.Hash {
	return func() hash.Hash {
		return newMessageCounter(startState, step)
	}
}

func sliceToElementSlice[T any](slice []T) ([]small_rational.SmallRational, error) {
	elementSlice := make([]small_rational.SmallRational, len(slice))
	for i, v := range slice {
		if _, err := elementSlice[i].SetInterface(v); err != nil {
			return nil, err
		}
	}
	return elementSlice, nil
}

func sliceEquals(a []small_rational.SmallRational, b []small_rational.SmallRational) error {
	if len(a) != len(b) {
		return fmt.Errorf("length mismatch %d≠%d", len(a), len(b))
	}
	for i := range a {
		if !a[i].Equal(&b[i]) {
			return fmt.Errorf("at index %d: %s ≠ %s", i, a[i].String(), b[i].String())
		}
	}
	return nil
}

func polynomialSliceEquals(a []polynomial.Polynomial, b []polynomial.Polynomial) error {
	if len(a) != len(b) {
		return fmt.Errorf("length mismatch %d≠%d", len(a), len(b))
	}
	for i := range a {
		if err := sliceEquals(a[i], b[i]); err != nil {
			return fmt.Errorf("at index %d: %w", i, err)
		}
	}
	return nil
}
