//go:build unix && !linux

package subproc

// killDescendants needs /proc; elsewhere the group kill is all there is.
func killDescendants(int) {}
