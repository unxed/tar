module github.com/unxed/tar

go 1.25.5

require (
	github.com/klauspost/compress v1.18.7-0.20260521203646-ecdb779d8745
	github.com/ncruces/go-sqlite3 v0.22.0
	github.com/ulikunitz/xz v0.5.11
	github.com/unxed/par2 v0.1.2
	golang.org/x/sync v0.10.0
	golang.org/x/sys v0.29.0
)

require (
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/tetratelabs/wazero v1.8.2 // indirect
)

replace github.com/ulikunitz/xz => github.com/unxed/xz v0.1.3
