/*
Copyright (c) 2018 Dropbox, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scorecard

import (
	"strings"
)

const (
	// TagJoiner is the default way to join a tag key and value
	TagJoiner = ":"
	// RuleDelimiter is the character used to join rules into composites
	RuleDelimiter = ";"
	// WildCard is used to specify a wildcard value
	WildCard = "*"
)

// TagMatchesRule returns whether the tag matches the rule according to the definition of
// "match" in scorecard.go.
func TagMatchesRule(t Tag, r Rule) bool {
	return TagMatchesPattern(t, r.Pattern)
}

func advanceTagIndex(ts string, index int) int {
	tsLen := len(ts)
	i := index
	for ; i < tsLen; i++ {
		if ts[i] == ';' {
			break
		}
	}
	return i - 1
}

// Function returns true if the tag matches the pattern.
// We keep two indexes one into the tag and the other into the pattern.
// We advance the indexes if the characters at the index match. If they
// don't match we return false. If the character in the pattern is a wildcard
// then we simply advance the index till the end of the fragment which is either
// the end of the entire string or till we hit the delimeter ';'
func TagMatchesPattern(t Tag, p string) bool {
	ts := string(t)
	tLen := len(ts)
	pLen := len(p)
	tIndex := 0 // Index into tag
	pIndex := 0 // Index into pattern
	for tIndex < tLen && pIndex < pLen {
		if p[pIndex] == '*' {
			// Advance the tag index till the end of this fragment
			tIndex = advanceTagIndex(ts, tIndex)
		} else if ts[tIndex] != p[pIndex] {
			return false
		}
		pIndex++
		tIndex++
	}

	// If we've exhausted both the tag and the pattern return true
	return pIndex == pLen && tIndex == tLen ||
		// special case a wild card at the end of the pattern
		(tIndex == tLen && pIndex == pLen-1 && p[pIndex] == '*')
}

// Matches is a helper to put TagMatchesRule as a member of Tag
func (t Tag) Matches(r Rule) bool {
	return TagMatchesRule(t, r)
}

// Matches is a helper to put TagMatchesRule as a member of Rule
func (r Rule) Matches(t Tag) bool {
	return TagMatchesRule(t, r)
}

// Helper keeping track of a) single fragment rules and b) compound
// rules. It allows to perform tag permutation for any compound rule
// that fully matches a given set of tags. This is used to expand
// a compound rule with glob patterns into multiple permutated variants.
type compoundTagGenerator struct {
	fragments        map[string][]fragmentPointer
	orderedFragments []*fragmentedRule
}

// This holds the fragments derived from the given rule
type fragmentedRule struct {
	fragments []string
	rule      Rule
}

// Point to a fragmented rule and a fragment within that rule
type fragmentPointer struct {
	fr  *fragmentedRule
	idx int
}

// Keep track of tags that match the fragments for the fragmented rule.
//
// The "matches" slice must be the same length as fr.fragments.  Each element of
// that slice is a slice of tags that matched the element.
//
// The resulting conjunction for this match state exists iff all matches slices
// are non-empty.  If they are non-empty, the tags are the cartesian product of
// the matches slice.
//
// e.g. matches=[][]Tag{{"op:read", "op:write"}, {"gid:13", "gid:42"}} generates:
// "op:read;gid:13"
// "op:read;gid:42"
// "op:write;gid:13"
// "op:write;gid:42"
// We could get that from a rule "op:*;gid:*"
type matchState struct {
	fr      *fragmentedRule
	matches [][]Tag
}

// productSize returns the size of the cartesian product of the given sets.
func productSize(outer [][]Tag) int {
	sum := 0
	prod := 1
	for _, inner := range outer {
		sum += len(inner)
		if len(inner) > 0 {
			prod *= len(inner)
		}
	}
	if sum == 0 {
		return 0
	}
	return prod
}

func (ms *matchState) generate() []Tag {
	// index into each slice in matches for purposes of cartesian product
	// calculation
	indices := make([]int, len(ms.matches))
	tags := make([]Tag, 0, productSize(ms.matches))
	var buf strings.Builder
	for !ms.permutationDone(indices) {
		for i := 0; i < len(ms.matches); i++ {
			if i > 0 {
				_, _ = buf.WriteString(RuleDelimiter)
			}
			_, _ = buf.WriteString(string(ms.matches[i][indices[i]]))
		}
		tags = append(tags, Tag(buf.String()))
		buf.Reset()
		ms.advancePermutation(indices)
	}
	return tags
}

func (ms *matchState) permutationDone(indices []int) bool {
	// This checks that all indices are in range, signifying a valid
	// permutation.
	//
	// The advancePermutation function will violate this only after exhausting
	// all permutations.
	for i := 0; i < len(ms.matches); i++ {
		if indices[i] >= len(ms.matches[i]) {
			return true
		}
	}
	return false
}

func (ms *matchState) advancePermutation(indices []int) {
	// Think of this function as acting like one of those spinning counters used
	// in automobile odometers.  It increments the least significant digit and
	// if that is less than the max value for that digit, is done.  If that
	// digit overflows and there is a next digit, it resets the current digit to
	// 0.  The loop will then advance the next least significant digit on its
	// next go 'round.  The loop exits when either a digit is less than its max
	// value, or the loop iterates through all digits (in which case, the
	// indices[i] is permitted to equal or exceed ms.matches[i]).
	for i := 0; i < len(indices); i++ {
		// go rtl
		idx := len(indices) - i - 1
		indices[idx]++
		if indices[idx] < len(ms.matches[idx]) {
			return
		} else if indices[idx] >= len(ms.matches[idx]) && i+1 < len(ms.matches) {
			indices[idx] = 0
		}
	}
}

// Generate the set of tags that could be compound tags for this rule set.
//
// This is equivalent to generating the cartesian product of tags with itself
// as many times as is necessary and then filtering the results to just those
// matched by rules.
//
// Each rule with 1+ fragments will, if fully matched (e.g if each fragment has
// at least one match), will expand into its permutations (e.g all tag values for
// a given fragment).
//
// The implementation is more efficient than the above description.
func (ctg *compoundTagGenerator) combine(tags []Tag) []Tag {
	matches := make(map[*fragmentedRule]*matchState)
	// fragments first so a ruleset without compound rules pays near-zero cost
	for pattern, fragmentPointers := range ctg.fragments {
		for _, tag := range tags {
			if TagMatchesPattern(tag, pattern) {
				// Each fragment pointer associated with the matched pattern
				// points to a rule and indexes the fragment in that rule.
				for _, fragmentPointer := range fragmentPointers {
					// If we don't have match state for the fragmented rule
					// pointed to by the fragmentPointer.
					if _, ok := matches[fragmentPointer.fr]; !ok {
						// Create the new match state and install it in matches
						matches[fragmentPointer.fr] = newMatchState(fragmentPointer.fr)
					}
					// Add this tag to the list for the fragmentPointer's index.
					st := matches[fragmentPointer.fr]
					st.matches[fragmentPointer.idx] = append(st.matches[fragmentPointer.idx], tag)
				}
			}
		}
	}

	// For each tag, construct the cartesian product of all its matched tags.
	// For example, the rule "op:*;gid:*" will generate a list of new tags along
	// the lines of:
	//      for t1 matching op:*
	//          for t2 matching gid:*
	//              yield t1 + ";" + t2
	// The generate function will conceptually have as many of these nested
	// loops as there are fragments in fr.
	product := make([]Tag, 0)
	for _, fr := range ctg.orderedFragments {
		if match, ok := matches[fr]; ok {
			// NOTE(opaugam) - for each permutation output a) the rule it
			// belongs to (e.g the current ordered fragment pointer) and b)
			// the permutation itself.
			product = append(product, match.generate()...)
		}
	}

	return product
}

func newMatchState(fr *fragmentedRule) *matchState {
	st := &matchState{}
	st.fr = fr
	st.matches = make([][]Tag, len(fr.fragments))
	for i := 0; i < len(st.matches); i++ {
		st.matches[i] = make([]Tag, 0)
	}
	return st
}

// This helper returns a wrapper which can generate compount tags
// from a set of basic tags.
func newCompoundTagGenerator(rules []Rule) *compoundTagGenerator {
	ctg := &compoundTagGenerator{}
	ctg.fragments = make(map[string][]fragmentPointer)
	ctg.orderedFragments = make([]*fragmentedRule, 0, len(rules))
	for _, rule := range rules {
		frags := strings.Split(rule.Pattern, RuleDelimiter)
		if len(frags) < 2 {
			continue
		}
		fr := &fragmentedRule{frags, rule}
		ctg.orderedFragments = append(ctg.orderedFragments, fr)
		for idx, f := range fr.fragments {
			if arr, ok := ctg.fragments[f]; ok {
				ctg.fragments[f] = append(arr, fragmentPointer{fr, idx})
			} else {
				ctg.fragments[f] = []fragmentPointer{{fr, idx}}
			}
		}
	}
	return ctg
}
