package extract

// Resolution values: how sure an extractor is that a call or invoke goes where
// the record says. They are also the stored edge confidences "exact", "alias"
// and "candidate".
const (
	ResExact      = "exact"      // the target is certain
	ResAlias      = "alias"      // reached through an alias
	ResCandidate  = "candidate"  // one of several possible targets, or a guess
	ResDynamic    = "dynamic"    // target only known at run time (dynamic records)
	ResUnresolved = "unresolved" // no target found: the record gives no edge
	ResResolved   = "resolved"   // synonym of exact (alias with reason "alias"), written by wammybash
)

// ReasonAlias is the reason that turns the synonym "resolved" into "alias".
const ReasonAlias = "alias"

// NormalizeResolution returns the canonical resolution of a record of type t
// with the given resolution and reason keys, and whether the record is valid
// for its type.
//
//	call:                    exact | alias | candidate, REQUIRED. "resolved"
//	                         is exact, or alias when reason is "alias". Anything
//	                         else (absent, unresolved, dynamic, unknown) is
//	                         invalid: ok is false.
//	invoke, dotsource,
//	import:                  optional. Absent, exact and resolved give exact
//	                         ("resolved" with reason "alias" gives alias);
//	                         alias and candidate stay as written; unresolved
//	                         and any unknown value give unresolved (no edge).
//	dynamic:                 always dynamic.
//	any other type:          "" and ok.
//
// ok is false only for a call without a usable resolution.
func NormalizeResolution(t, resolution, reason string) (res string, ok bool) {
	canon := func() string {
		switch resolution {
		case ResExact, ResAlias, ResCandidate:
			return resolution
		case ResResolved:
			if reason == ReasonAlias {
				return ResAlias
			}
			return ResExact
		}
		return ""
	}
	switch t {
	case TypeCall:
		c := canon()
		return c, c != ""
	case TypeInvoke, TypeDotSource, TypeImport:
		if resolution == "" {
			return ResExact, true
		}
		if c := canon(); c != "" {
			return c, true
		}
		return ResUnresolved, true
	case TypeDynamic:
		return ResDynamic, true
	}
	return "", true
}

// Strength ranks a resolution or confidence for merging: exact (3) beats alias
// (2) beats candidate (1); everything else is 0.
func Strength(res string) int {
	switch res {
	case ResExact:
		return 3
	case ResAlias:
		return 2
	case ResCandidate:
		return 1
	}
	return 0
}
