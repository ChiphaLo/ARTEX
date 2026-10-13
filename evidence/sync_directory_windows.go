package evidence

// Windows cannot FlushFileBuffers on Go's read-only directory handle; attempting
// it fails every successful evidence binding with Access Denied. The body file
// is still fsynced before the same-volume atomic rename. Directory metadata
// flush has no portable Windows equivalent (unlike the Unix implementation).
func syncEvidenceDirectory(string) error { return nil }
