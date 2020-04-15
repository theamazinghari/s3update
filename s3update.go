package s3update

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mitchellh/ioprogress"
)

var remoteFileSize int64

type Updater struct {
	// CurrentVersion represents the current binary version.
	// This is generally set at the compilation time with -ldflags "-X main.Version=42"
	// See the README for additional information
	CurrentVersion string
	// S3Bucket represents the S3 bucket containing the different files used by s3update.
	S3Bucket string
	// S3Region represents the S3 region you want to work in.
	S3Region string
	// S3ReleaseKey represents the raw key on S3 to download new versions.
	// The value can be something like `cli/releases/cli-{{OS}}-{{ARCH}}`
	S3ReleaseKey string
	// S3VersionKey represents the key on S3 to download the current version
	S3VersionKey string
}

// validate ensures every required fields is correctly set. Otherwise and error is returned.
func (u Updater) validate() error {
	if u.CurrentVersion == "" {
		return fmt.Errorf("no version set")
	}

	if u.S3Bucket == "" {
		return fmt.Errorf("no bucket set")
	}

	if u.S3Region == "" {
		return fmt.Errorf("no s3 region")
	}

	if u.S3ReleaseKey == "" {
		return fmt.Errorf("no s3ReleaseKey set")
	}

	if u.S3VersionKey == "" {
		return fmt.Errorf("no s3VersionKey set")
	}

	return nil
}

// AutoUpdate runs synchronously a verification to ensure the binary is up-to-date.
// If a new version gets released, the download will happen automatically
// It's possible to bypass this mechanism by setting the S3UPDATE_DISABLED environment variable.
func AutoUpdate(u Updater) error {
	if os.Getenv("S3UPDATE_DISABLED") != "" {
		fmt.Println("s3update: autoupdate disabled")
		return nil
	}

	if err := u.validate(); err != nil {
		fmt.Printf("s3update: %s - skipping auto update\n", err.Error())
		return err
	}

	return runAutoUpdate(u)
}

// generateS3ReleaseKey dynamically builds the S3 key depending on the os and architecture.
func generateS3ReleaseKey(path string) string {
	path = strings.Replace(path, "{{OS}}", runtime.GOOS, -1)
	path = strings.Replace(path, "{{ARCH}}", runtime.GOARCH, -1)

	return path
}

func runAutoUpdate(u Updater) error {
	localVersion, err := strconv.ParseInt(u.CurrentVersion, 10, 64)
	if err != nil || localVersion == 0 {
		return fmt.Errorf("invalid local version")
	}

	svc := s3.New(session.Must(session.NewSession()), &aws.Config{Region: aws.String(u.S3Region)})
	resp, err := svc.GetObject(&s3.GetObjectInput{Bucket: aws.String(u.S3Bucket), Key: aws.String(u.S3VersionKey)})
	if err != nil {
		return err
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	remoteVersion, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || remoteVersion == 0 {
		return fmt.Errorf("invalid remote version")
	}

	fmt.Printf("s3update: Local Version %d - Remote Version: %d\n", localVersion, remoteVersion)
	if localVersion < remoteVersion {
		fmt.Printf("s3update: version outdated ... \n")
		s3Key := generateS3ReleaseKey(u.S3ReleaseKey)
		resp, err := svc.GetObject(&s3.GetObjectInput{Bucket: aws.String(u.S3Bucket), Key: aws.String(s3Key)})
		if err != nil {
			return err
		}
		remoteFileSize = *resp.ContentLength
		progressR := &ioprogress.Reader{
			Reader:       resp.Body,
			Size:         *resp.ContentLength,
			DrawInterval: 500 * time.Millisecond,
			DrawFunc: ioprogress.DrawTerminalf(os.Stdout, func(progress, total int64) string {
				bar := ioprogress.DrawTextFormatBar(40)
				return fmt.Sprintf("%s %20s", bar(progress, total), ioprogress.DrawTextFormatBytes(progress, total))
			}),
		}

		dest, err := os.Executable()
		if err != nil {
			return err
		}

		destBackup := dest + ".bak"

		// Create a temp file
		tempFile, err := ioutil.TempFile("", "s3update_tmp_download")
		if err != nil {
			return err
		}
		tempFilePath := tempFile.Name()

		// Download to tempFile
		// Use the same flags that ioutil.WriteFile uses
		f, err := os.OpenFile(tempFile.Name(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			_ = os.Remove(tempFile.Name())
			return err
		}

		if err := tempFile.Close(); err != nil {
			return err
		}

		if _, err := io.Copy(f, progressR); err != nil {
			return err
		}

		// Close the response stream
		if err := resp.Body.Close(); err != nil {
			return err
		}

		// The file must be closed so we can execute it in the next step
		if err := f.Close(); err != nil {
			return err
		}

		if err := finalizeUpdate(dest, destBackup, tempFilePath); err != nil {
			return err
		}

		fmt.Printf("s3update: updated with success to version %d\nRestarting application\n", remoteVersion)

		// The update completed, we can now restart the application without requiring any user action.
		if err := syscall.Exec(dest, os.Args, os.Environ()); err != nil {
			fmt.Println(err)
			return err
		}

		os.Exit(0)
	}
	return nil
}

func finalizeUpdate(originalFilePath, backupFilePath, tempFilePath string) (err error) {
	if downloadSucceeded(tempFilePath) {
		// Backup current binary
		if _, err = os.Stat(originalFilePath); err == nil {
			err = os.Rename(originalFilePath, backupFilePath)
		}

		// Replace old binary by downloaded file
		if err = os.Rename(tempFilePath, originalFilePath); err != nil {
			// revert backup file
			err = os.Rename(backupFilePath, originalFilePath)
		}
	} else { // Do nothing
		return fmt.Errorf("inconsistent file size")
	}
	return
}

func downloadSucceeded(tempFile string) bool {
	return fileSize(tempFile) == remoteFileSize
}

func fileSize(path string) int64 {
	fileInfo, err := os.Stat(path)
	if err != nil {
		log.Fatal(err)
	}
	return fileInfo.Size()
}
