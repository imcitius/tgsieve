package tfplan

import "github.com/imcitius/tgsieve/internal/attrpath"

// Attribute paths are built and parsed by internal/attrpath; these aliases
// keep the plan parser readable at the call sites.
var (
	JoinKey   = attrpath.JoinKey
	JoinIndex = attrpath.JoinIndex
	SplitPath = attrpath.SplitPath
	Ancestors = attrpath.Ancestors
	PathLess  = attrpath.PathLess
)
