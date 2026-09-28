//go:build !windows

package userworker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/benice2me11/codexify-go/internal/supervisor"
)

type Factory struct {
	Executable string
	ConfigPath string
	Env        map[string]string
	Log        *slog.Logger
}

func (f *Factory) Start(context.Context) (supervisor.Process, error) {
	return nil, errors.New("user-context worker launcher is currently implemented only on Windows")
}
