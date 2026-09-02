package fixtures

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

// ParseSelectionSets is the query parser the allowlist is projected from, so a test can
// check what it refuses without going through a whole capture.
var ParseSelectionSets = parseSelectionSets
