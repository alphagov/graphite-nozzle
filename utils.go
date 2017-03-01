package main

import (
	"encoding/json"
	"fmt"
)

// MetricVars will contain the variables the tenant could use to compose their
// custom metric namespace.
type MetricVars struct {
	App          string
	GUID         string
	Index        string
	Job          string
	Metric       string
	Organisation string
	Space        string
}

func deb(v interface{}) {
	b, _ := json.Marshal(v)
	fmt.Printf("\n\n\n%s\n\n\n", string(b))
}

// http://stackoverflow.com/questions/19374219/how-to-find-the-difference-between-two-slices-of-strings-in-golang
func difference(slice1 []string, slice2 []string) []string {
	var diff []string

	// Loop two times, first to find slice1 strings not in slice2,
	// second loop to find slice2 strings not in slice1
	for i := 0; i < 2; i++ {
		for _, s1 := range slice1 {
			found := false
			for _, s2 := range slice2 {
				if s1 == s2 {
					found = true
					break
				}
			}
			// String not found. We add it to return slice
			if !found {
				diff = append(diff, s1)
			}
		}
		// Swap the slices, only if it was the first loop
		if i == 0 {
			slice1, slice2 = slice2, slice1
		}
	}

	return diff
}
