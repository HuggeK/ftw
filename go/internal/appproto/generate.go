package appproto

// The wire constants live in package protocol, which FTW Cloud imports too.
// The generator stays here, in the box, and writes across to it.

//go:generate go run ./gencontract/cmd proto ../../../contract/registry.yaml ../../protocol/contract_gen.go
