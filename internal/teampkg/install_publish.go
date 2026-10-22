package teampkg

import "github.com/kjelly/hufu/internal/fsutil"

func publishDirectoryNoReplace(source, target string) error {
	return fsutil.PublishDirectoryNoReplace(source, target)
}
