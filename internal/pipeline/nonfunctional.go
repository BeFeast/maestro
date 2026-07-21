package pipeline

// Non-functional change classification (#1020). A merged PR that touches only
// non-functional paths (documentation, QA records) must not be treated as a
// fix that settles a bug issue. The helpers here are deterministic and
// side-effect free; they reuse the doublestar-style glob matcher from
// MatchesVisualPath so path semantics stay identical across the codebase.

// DefaultNonFunctionalPaths lists the glob patterns whose exclusive change in a
// merged PR is a documentation/record delivery rather than a code fix. Projects
// may extend this set via supervisor.non_functional_paths; docs/** is always
// included.
var DefaultNonFunctionalPaths = []string{"docs/**"}

// MatchesPathGlob reports whether a repo-relative file path matches a single
// doublestar-style glob pattern. It is a semantic alias for MatchesVisualPath
// so callers classifying non-functional paths do not read as visual-evidence
// logic.
func MatchesPathGlob(pattern, file string) bool {
	return MatchesVisualPath(pattern, file)
}

// AllPathsNonFunctional reports whether every changed path matches at least one
// of the supplied non-functional globs. An empty changedFiles set returns false:
// "no diff" is not evidence of a documentation-only delivery and must never
// release an issue on its own. An empty patterns set falls back to
// DefaultNonFunctionalPaths so a caller that forgot to configure the set still
// gets the docs/** default rather than classifying everything as non-functional.
func AllPathsNonFunctional(patterns, changedFiles []string) bool {
	if len(changedFiles) == 0 {
		return false
	}
	globs := patterns
	if len(globs) == 0 {
		globs = DefaultNonFunctionalPaths
	}
	for _, file := range changedFiles {
		matched := false
		for _, pattern := range globs {
			if MatchesPathGlob(pattern, file) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
