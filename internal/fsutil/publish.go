package fsutil

// PublishDirectoryNoReplace atomically renames a prepared directory to a new
// sibling destination. It refuses an existing destination, including an empty
// directory. Source and destination must share a filesystem. The caller owns
// syncing content before publication and syncing the parent afterwards.
func PublishDirectoryNoReplace(source, target string) error {
	return publishDirectoryNoReplace(source, target)
}
