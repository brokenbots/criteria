package runtime

import "errors"

// ErrTerminal signals that the interpreter reached a terminal node.
var ErrTerminal = errors.New("terminal node reached")

// ErrPaused signals that execution has paused and can be resumed later.
var ErrPaused = errors.New("run paused")
