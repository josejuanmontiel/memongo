package mongobin

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/afero"
	"github.com/tryvium-travels/memongo/memongolog"
)

var Afs afero.Afero

func init() {
	Afs = afero.Afero{
		Fs: afero.NewOsFs(),
	}
}

func MoveFile(sourcePath, destPath string) error {
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't open source file: %s", err)
	}
	outputFile, err := os.Create(destPath)
	if err != nil {
		inputFile.Close()
		return fmt.Errorf("Couldn't open dest file: %s", err)
	}
	defer outputFile.Close()
	_, err = io.Copy(outputFile, inputFile)

	inputFile.Close()
	if err != nil {
		return fmt.Errorf("Writing to output file failed: %s", err)
	}
	// The copy was successful, so now delete the original file
	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("Failed removing original file: %s", err)
	}
	return nil
}

// GetOrDownloadMongod returns the path to the mongod binary from the tarball
// at the given URL. If the URL has not yet been downloaded, it's downloaded
// and saved the the cache. If it has been downloaded, the existing mongod
// path is returned.
func GetOrDownloadMongod(urlStr string, cachePath string, logger *memongolog.Logger) (string, error) {
	dirname, dirErr := directoryNameForURL(urlStr)
	if dirErr != nil {
		return "", dirErr
	}

	dirPath := path.Join(cachePath, dirname)
	mongodPath := path.Join(dirPath, "mongod")

	// Check the cache
	existsInCache, existsErr := Afs.Exists(mongodPath)
	if existsErr != nil {
		return "", fmt.Errorf("error while checking for mongod in cache: %s", existsErr)
	}
	if existsInCache {
		logger.Debugf("mongod from %s exists in cache at %s", urlStr, mongodPath)
		return mongodPath, nil
	}

	logger.Infof("mongod from %s does not exist in cache, downloading to %s", urlStr, mongodPath)
	downloadStartTime := time.Now()

	// Download the file
	// nolint:gosec
	resp, httpGetErr := http.Get(urlStr)
	if httpGetErr != nil {
		return "", fmt.Errorf("error getting tarball from %s: %s", urlStr, httpGetErr)
	}
	defer resp.Body.Close()

	tgzTempFile, tmpFileErr := Afs.TempFile("", "")
	if tmpFileErr != nil {
		return "", fmt.Errorf("error creating temp file for tarball: %s", tmpFileErr)
	}
	defer func() {
		_ = tgzTempFile.Close()
		_ = Afs.Remove(tgzTempFile.Name())
	}()

	_, copyErr := io.Copy(tgzTempFile, resp.Body)
	if copyErr != nil {
		return "", fmt.Errorf("error downloading tarball from %s: %s", urlStr, copyErr)
	}

	_, seekErr := tgzTempFile.Seek(0, 0)
	if seekErr != nil {
		return "", fmt.Errorf("error seeking back to start of file: %s", seekErr)
	}

	// Extract mongod
	gzReader, gzErr := gzip.NewReader(tgzTempFile)
	if gzErr != nil {
		return "", fmt.Errorf("error intializing gzip reader from %s: %s", tgzTempFile.Name(), gzErr)
	}

	tarReader := tar.NewReader(gzReader)

	const maxFilesCount = 2
	fileCount := 0
	for {
		nextFile, tarErr := tarReader.Next()
		if tarErr == io.EOF {
			return "", fmt.Errorf("did not find a mongod binary in the tar from %s", urlStr)
		}
		if tarErr != nil {
			return "", fmt.Errorf("error reading from tar: %s", tarErr)
		}

		if strings.HasSuffix(nextFile.Name, "bin/mongod") || strings.HasSuffix(nextFile.Name, "bin/mongo") {
			fileCount++
			err := saveFile(path.Join(dirPath, filepath.Base(nextFile.Name)), tarReader, logger)
			if err != nil {
				return "", err
			}
			if fileCount >= maxFilesCount {
				break
			}
		}
	}

	logger.Infof("finished downloading mongod to %s in %s", mongodPath, time.Since(downloadStartTime).String())

	return mongodPath, nil
}

func saveFile(mongodPath string, tarReader *tar.Reader, logger *memongolog.Logger) error {
	mkdirErr := Afs.MkdirAll(path.Dir(mongodPath), 0755)
	if mkdirErr != nil {
		return fmt.Errorf("error creating directory %s: %s", path.Dir(mongodPath), mkdirErr)
	}

	// Extract to a temp file first, then copy to the destination, so we get
	// atomic behavior if there's multiple parallel downloaders
	mongodTmpFile, tmpFileErr := Afs.TempFile("", "")
	if tmpFileErr != nil {
		return fmt.Errorf("error creating temp file for mongod: %s", tmpFileErr)
	}
	defer func() {
		_ = mongodTmpFile.Close()
	}()

	_, writeErr := io.Copy(mongodTmpFile, tarReader)
	if writeErr != nil {
		return fmt.Errorf("error writing mongod binary at %s: %s", mongodTmpFile.Name(), writeErr)
	}

	_ = mongodTmpFile.Close()

	chmodErr := Afs.Chmod(mongodTmpFile.Name(), 0755)
	if chmodErr != nil {
		return fmt.Errorf("error chmod-ing mongodb binary at %s: %s", mongodTmpFile, chmodErr)
	}

	renameErr := Afs.Rename(mongodTmpFile.Name(), mongodPath)
	if renameErr != nil {
		linkErr := &os.LinkError{}
		if errors.As(renameErr, &linkErr) {
			// If /tmp is on another filesystem, we have to copy the file instead.

			logger.Debugf("Going to move %s to %s, copying instead", mongodTmpFile.Name(), mongodPath)
			MoveFile(mongodTmpFile.Name(), mongodPath)

			chmodErr := Afs.Chmod(mongodPath, 0755)
			if chmodErr != nil {
				return fmt.Errorf("error chmod-ing mongodb binary at %s: %s", mongodPath, chmodErr)
			}

		}

	}
	return nil
}

// After the download a tarball, we extract it to a directory in the cache.
// We want the name of this directory to be both human-redable, and also
// unique (no two URLs should have the same directory name). We can't just
// use the name of the tarball, because the URL can be passed in by the
// user (so https://mongodb.org/dl/linux/foobar.tgz has to have a different
// path than https://mymirror.org/dl/linux/foobar.tgz).
//
// To meet these requirements, we name the directory <basename>_<hash>, where
// basname is the last path element of the URL stripped of any non-path-safe
// characters, and the hash is the first 10 characters of the sha256 checksum of
// the whole URL.
func directoryNameForURL(urlStr string) (string, error) {
	shasum := sha256.New()
	_, _ = shasum.Write([]byte(urlStr))

	shahex := hex.EncodeToString(shasum.Sum(nil))
	hash := shahex[0:10]

	urlParsed, parseErr := url.Parse(urlStr)
	if parseErr != nil {
		return "", fmt.Errorf("could not parse url: %s", parseErr)
	}

	basename := sanitizeFilename(path.Base(urlParsed.Path))

	return fmt.Sprintf("%s_%s", basename, hash), nil
}

var filenameUnsafeCharRegex = regexp.MustCompile("[^a-zA-Z0-9_-]")

func sanitizeFilename(unsanitized string) string {
	return filenameUnsafeCharRegex.ReplaceAllString(unsanitized, "_")
}
