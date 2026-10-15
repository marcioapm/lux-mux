// Package ui is luxmux's full-screen dialogs: the window chooser and the
// new-window dialog.
package ui

import "github.com/marcioapm/lux-mux/internal/mux"

// New is the dialogs; nil until they exist.
func New() mux.UI { return nil }
