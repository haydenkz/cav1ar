package encode

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// VideoExts are the file extensions treated as video.
var VideoExts = []string{
	".3gp", ".avi", ".flv", ".m2ts", ".m4v", ".mkv", ".mov", ".mp4", ".mpeg",
	".mpg", ".mts", ".ogv", ".ts", ".vob", ".webm", ".wmv", ".y4m",
}

// IsVideo reports whether path looks like a video file.
func IsVideo(path string) bool {
	return slices.Contains(VideoExts, strings.ToLower(filepath.Ext(path)))
}

// FindVideos returns every video under root, including in subfolders, in
// sorted order. Hidden files and folders are skipped, which includes results
// still being written.
func FindVideos(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // skip unreadable folders rather than giving up
		}
		hidden := path != root && strings.HasPrefix(d.Name(), ".")
		switch {
		case d.IsDir() && hidden:
			return filepath.SkipDir
		case !d.IsDir() && !hidden && IsVideo(path):
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// fileID tells whether a file is still the one a job started on: the same
// file, the same size, and not written to since.
type fileID struct {
	dev, ino uint64
	size     int64
	mod      unix.Timespec
}

// identify reads path's fileID, and how many hard links it has.
func identify(path string) (id fileID, links uint64, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fileID{}, 0, err
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino, size: st.Size, mod: st.Mtim}, uint64(st.Nlink), nil
}

// unchanged reports whether path is still the file id describes.
func unchanged(path string, id fileID) bool {
	now, _, err := identify(path)
	return err == nil && now == id
}
