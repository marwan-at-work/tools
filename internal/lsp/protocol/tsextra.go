// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package protocol

// This file contains extra types defined by
// the protocol but are not yet auto generated

// TODO: remove this file once we can generate progress types from here: https://github.com/microsoft/vscode-languageserver-node/blob/master/protocol/src/protocol.progress.ts

type WorkDoneProgressBegin struct {
	Kind        string `json:"kind,omitempty"`
	Title       string `json:"title,omitempty"`
	Cancellable bool   `json:"cancellable,omitempty"`
	Message     string `json:"message,omitempty"`
	Percentage  int    `json:"percentage,omitempty"`
}

type WorkDoneProgressReport struct {
	Kind        string `json:"kind,omitempty"`
	Cancellable bool   `json:"cancellable,omitempty"`
	Message     string `json:"message,omitempty"`
	Percentage  int    `json:"percentage,omitempty"`
}

type WorkDoneProgressEnd struct {
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}
