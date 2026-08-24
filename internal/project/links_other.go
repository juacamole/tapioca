//go:build !unix

package project

import "os"

// links has no portable answer off Unix, and reporting one name is the answer
// that costs nothing: the walk it gates is a defence against a hard link, and
// a platform that cannot be asked about link counts is not one to spend a
// directory walk per instruction file on.
func links(os.FileInfo) uint64 { return 1 }
