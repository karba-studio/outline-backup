module github.com/karba-studio/outline-backup

go 1.22

// No third-party dependencies, on purpose. This tool handles backup credentials
// and encryption keys; every import here is the Go standard library.
