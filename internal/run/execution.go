package run

// SourceProfile and ResourceSpecVersion identify the current qualified execution
// contract. Historical resource decoding remains a storage/provider concern;
// recognizing an older resource never authorizes new execution with it.
const (
	SourceProfile       = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	ResourceSpecVersion = 10
)
