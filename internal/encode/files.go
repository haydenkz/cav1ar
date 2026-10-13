package encode

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
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

// IsEncode reports whether path looks like it is already AV1, going by an
// "AV1" in its name as cav1ar's own output names have.
func IsEncode(path string) bool {
	return av1Word.MatchString(baseName(path))
}

// FindVideos returns every video under root, including in subfolders, in
// sorted order. Hidden folders and files that are already AV1 are skipped.
func FindVideos(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // skip unreadable folders rather than giving up
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if IsVideo(path) && !IsEncode(path) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}
