package overseer

import pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"

// ForEachEntered is emitted when execution enters a for_each node.
type ForEachEntered = pb.ForEachEntered

// ForEachIteration is emitted at the start of each iteration of a for_each loop.
type ForEachIteration = pb.ForEachIteration

// ForEachOutcome is emitted when a for_each loop completes.
type ForEachOutcome = pb.ForEachOutcome

// ScopeIterCursorSet is emitted when the loop-iteration cursor variable is written.
type ScopeIterCursorSet = pb.ScopeIterCursorSet
