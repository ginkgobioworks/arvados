package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/curoverse/azure-sdk-for-go/storage"
)

var (
	azureMaxGetBytes           int
	azureStorageAccountName    string
	azureStorageAccountKeyFile string
	azureStorageReplication    int
	azureWriteRaceInterval     = 15 * time.Second
	azureWriteRacePollTime     = time.Second
)

func readKeyFromFile(file string) (string, error) {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return "", errors.New("reading key from " + file + ": " + err.Error())
	}
	accountKey := strings.TrimSpace(string(buf))
	if accountKey == "" {
		return "", errors.New("empty account key in " + file)
	}
	return accountKey, nil
}

type azureVolumeAdder struct {
	*volumeSet
}

func (s *azureVolumeAdder) Set(containerName string) error {
	if trashLifetime != 0 {
		return ErrNotImplemented
	}

	if containerName == "" {
		return errors.New("no container name given")
	}
	if azureStorageAccountName == "" || azureStorageAccountKeyFile == "" {
		return errors.New("-azure-storage-account-name and -azure-storage-account-key-file arguments must given before -azure-storage-container-volume")
	}
	accountKey, err := readKeyFromFile(azureStorageAccountKeyFile)
	if err != nil {
		return err
	}
	azClient, err := storage.NewBasicClient(azureStorageAccountName, accountKey)
	if err != nil {
		return errors.New("creating Azure storage client: " + err.Error())
	}
	if flagSerializeIO {
		log.Print("Notice: -serialize is not supported by azure-blob-container volumes.")
	}
	v := NewAzureBlobVolume(azClient, containerName, flagReadonly, azureStorageReplication)
	if err := v.Check(); err != nil {
		return err
	}
	*s.volumeSet = append(*s.volumeSet, v)
	return nil
}

func init() {
	flag.Var(&azureVolumeAdder{&volumes},
		"azure-storage-container-volume",
		"Use the given container as a storage volume. Can be given multiple times.")
	flag.StringVar(
		&azureStorageAccountName,
		"azure-storage-account-name",
		"",
		"Azure storage account name used for subsequent --azure-storage-container-volume arguments.")
	flag.StringVar(
		&azureStorageAccountKeyFile,
		"azure-storage-account-key-file",
		"",
		"File containing the account key used for subsequent --azure-storage-container-volume arguments.")
	flag.IntVar(
		&azureStorageReplication,
		"azure-storage-replication",
		3,
		"Replication level to report to clients when data is stored in an Azure container.")
	flag.IntVar(
		&azureMaxGetBytes,
		"azure-max-get-bytes",
		BlockSize,
		fmt.Sprintf("Maximum bytes to request in a single GET request. If smaller than %d, use multiple concurrent range requests to retrieve a block.", BlockSize))
}

// An AzureBlobVolume stores and retrieves blocks in an Azure Blob
// container.
type AzureBlobVolume struct {
	azClient      storage.Client
	bsClient      storage.BlobStorageClient
	containerName string
	readonly      bool
	replication   int
}

// NewAzureBlobVolume returns a new AzureBlobVolume using the given
// client and container name. The replication argument specifies the
// replication level to report when writing data.
func NewAzureBlobVolume(client storage.Client, containerName string, readonly bool, replication int) *AzureBlobVolume {
	return &AzureBlobVolume{
		azClient:      client,
		bsClient:      client.GetBlobService(),
		containerName: containerName,
		readonly:      readonly,
		replication:   replication,
	}
}

// Check returns nil if the volume is usable.
func (v *AzureBlobVolume) Check() error {
	ok, err := v.bsClient.ContainerExists(v.containerName)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("container does not exist")
	}
	return nil
}

// Return true if expires_at metadata attribute is found on the block
func (v *AzureBlobVolume) checkTrashed(loc string) (bool, map[string]string, error) {
	metadata, err := v.bsClient.GetBlobMetadata(v.containerName, loc)
	if err != nil {
		return false, metadata, v.translateError(err)
	}
	if metadata["expires_at"] != "" {
		return true, metadata, nil
	}
	return false, metadata, nil
}

// Get reads a Keep block that has been stored as a block blob in the
// container.
//
// If the block is younger than azureWriteRaceInterval and is
// unexpectedly empty, assume a PutBlob operation is in progress, and
// wait for it to finish writing.
func (v *AzureBlobVolume) Get(loc string, buf []byte) (int, error) {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return 0, err
	}
	if trashed {
		return 0, os.ErrNotExist
	}
	var deadline time.Time
	haveDeadline := false
	size, err := v.get(loc, buf)
	for err == nil && size == 0 && loc != "d41d8cd98f00b204e9800998ecf8427e" {
		// Seeing a brand new empty block probably means we're
		// in a race with CreateBlob, which under the hood
		// (apparently) does "CreateEmpty" and "CommitData"
		// with no additional transaction locking.
		if !haveDeadline {
			t, err := v.Mtime(loc)
			if err != nil {
				log.Print("Got empty block (possible race) but Mtime failed: ", err)
				break
			}
			deadline = t.Add(azureWriteRaceInterval)
			if time.Now().After(deadline) {
				break
			}
			log.Printf("Race? Block %s is 0 bytes, %s old. Polling until %s", loc, time.Since(t), deadline)
			haveDeadline = true
		} else if time.Now().After(deadline) {
			break
		}
		time.Sleep(azureWriteRacePollTime)
		size, err = v.get(loc, buf)
	}
	if haveDeadline {
		log.Printf("Race ended with size==%d", size)
	}
	return size, err
}

func (v *AzureBlobVolume) get(loc string, buf []byte) (int, error) {
	expectSize := len(buf)
	if azureMaxGetBytes < BlockSize {
		// Unfortunately the handler doesn't tell us how long the blob
		// is expected to be, so we have to ask Azure.
		props, err := v.bsClient.GetBlobProperties(v.containerName, loc)
		if err != nil {
			return 0, v.translateError(err)
		}
		if props.ContentLength > int64(BlockSize) || props.ContentLength < 0 {
			return 0, fmt.Errorf("block %s invalid size %d (max %d)", loc, props.ContentLength, BlockSize)
		}
		expectSize = int(props.ContentLength)
	}

	if expectSize == 0 {
		return 0, nil
	}

	// We'll update this actualSize if/when we get the last piece.
	actualSize := -1
	pieces := (expectSize + azureMaxGetBytes - 1) / azureMaxGetBytes
	errors := make([]error, pieces)
	var wg sync.WaitGroup
	wg.Add(pieces)
	for p := 0; p < pieces; p++ {
		go func(p int) {
			defer wg.Done()
			startPos := p * azureMaxGetBytes
			endPos := startPos + azureMaxGetBytes
			if endPos > expectSize {
				endPos = expectSize
			}
			var rdr io.ReadCloser
			var err error
			if startPos == 0 && endPos == expectSize {
				rdr, err = v.bsClient.GetBlob(v.containerName, loc)
			} else {
				rdr, err = v.bsClient.GetBlobRange(v.containerName, loc, fmt.Sprintf("%d-%d", startPos, endPos-1), nil)
			}
			if err != nil {
				errors[p] = err
				return
			}
			defer rdr.Close()
			n, err := io.ReadFull(rdr, buf[startPos:endPos])
			if pieces == 1 && (err == io.ErrUnexpectedEOF || err == io.EOF) {
				// If we don't know the actual size,
				// and just tried reading 64 MiB, it's
				// normal to encounter EOF.
			} else if err != nil {
				errors[p] = err
			}
			if p == pieces-1 {
				actualSize = startPos + n
			}
		}(p)
	}
	wg.Wait()
	for _, err := range errors {
		if err != nil {
			return 0, v.translateError(err)
		}
	}
	return actualSize, nil
}

// Compare the given data with existing stored data.
func (v *AzureBlobVolume) Compare(loc string, expect []byte) error {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return err
	}
	if trashed {
		return os.ErrNotExist
	}
	rdr, err := v.bsClient.GetBlob(v.containerName, loc)
	if err != nil {
		return v.translateError(err)
	}
	defer rdr.Close()
	return compareReaderWithBuf(rdr, expect, loc[:32])
}

// Put stores a Keep block as a block blob in the container.
func (v *AzureBlobVolume) Put(loc string, block []byte) error {
	if v.readonly {
		return MethodDisabledError
	}
	return v.bsClient.CreateBlockBlobFromReader(v.containerName, loc, uint64(len(block)), bytes.NewReader(block), nil)
}

// Touch updates the last-modified property of a block blob.
func (v *AzureBlobVolume) Touch(loc string) error {
	if v.readonly {
		return MethodDisabledError
	}
	trashed, metadata, err := v.checkTrashed(loc)
	if err != nil {
		return err
	}
	if trashed {
		return os.ErrNotExist
	}

	metadata["touch"] = fmt.Sprintf("%d", time.Now())
	return v.bsClient.SetBlobMetadata(v.containerName, loc, metadata, nil)
}

// Mtime returns the last-modified property of a block blob.
func (v *AzureBlobVolume) Mtime(loc string) (time.Time, error) {
	trashed, _, err := v.checkTrashed(loc)
	if err != nil {
		return time.Time{}, err
	}
	if trashed {
		return time.Time{}, os.ErrNotExist
	}

	props, err := v.bsClient.GetBlobProperties(v.containerName, loc)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC1123, props.LastModified)
}

// IndexTo writes a list of Keep blocks that are stored in the
// container.
func (v *AzureBlobVolume) IndexTo(prefix string, writer io.Writer) error {
	params := storage.ListBlobsParameters{
		Prefix:  prefix,
		Include: "metadata",
	}
	for {
		resp, err := v.bsClient.ListBlobs(v.containerName, params)
		if err != nil {
			return err
		}
		for _, b := range resp.Blobs {
			t, err := time.Parse(time.RFC1123, b.Properties.LastModified)
			if err != nil {
				return err
			}
			if !v.isKeepBlock(b.Name) {
				continue
			}
			if b.Properties.ContentLength == 0 && t.Add(azureWriteRaceInterval).After(time.Now()) {
				// A new zero-length blob is probably
				// just a new non-empty blob that
				// hasn't committed its data yet (see
				// Get()), and in any case has no
				// value.
				continue
			}
			if b.Metadata["expires_at"] != "" {
				// Trashed blob; exclude it from response
				continue
			}
			fmt.Fprintf(writer, "%s+%d %d\n", b.Name, b.Properties.ContentLength, t.UnixNano())
		}
		if resp.NextMarker == "" {
			return nil
		}
		params.Marker = resp.NextMarker
	}
}

// Trash a Keep block.
func (v *AzureBlobVolume) Trash(loc string) error {
	if v.readonly {
		return MethodDisabledError
	}

	// Ideally we would use If-Unmodified-Since, but that
	// particular condition seems to be ignored by Azure. Instead,
	// we get the Etag before checking Mtime, and use If-Match to
	// ensure we don't delete data if Put() or Touch() happens
	// between our calls to Mtime() and DeleteBlob().
	props, err := v.bsClient.GetBlobProperties(v.containerName, loc)
	if err != nil {
		return err
	}
	if t, err := v.Mtime(loc); err != nil {
		return err
	} else if time.Since(t) < blobSignatureTTL {
		return nil
	}

	// If trashLifetime == 0, just delete it
	if trashLifetime == 0 {
		return v.bsClient.DeleteBlob(v.containerName, loc, map[string]string{
			"If-Match": props.Etag,
		})
	}

	// Otherwise, mark as trash
	return v.bsClient.SetBlobMetadata(v.containerName, loc, map[string]string{
		"expires_at": fmt.Sprintf("%d", time.Now().Add(trashLifetime).Unix()),
	}, map[string]string{
		"If-Match": props.Etag,
	})
}

// Untrash a Keep block.
// Delete the expires_at metadata attribute
func (v *AzureBlobVolume) Untrash(loc string) error {
	// if expires_at does not exist, return NotFoundError
	metadata, err := v.bsClient.GetBlobMetadata(v.containerName, loc)
	if err != nil {
		return v.translateError(err)
	}
	if metadata["expires_at"] == "" {
		return os.ErrNotExist
	}

	// reset expires_at metadata attribute
	metadata["expires_at"] = ""
	err = v.bsClient.SetBlobMetadata(v.containerName, loc, metadata, nil)
	return v.translateError(err)
}

// Status returns a VolumeStatus struct with placeholder data.
func (v *AzureBlobVolume) Status() *VolumeStatus {
	return &VolumeStatus{
		DeviceNum: 1,
		BytesFree: BlockSize * 1000,
		BytesUsed: 1,
	}
}

// String returns a volume label, including the container name.
func (v *AzureBlobVolume) String() string {
	return fmt.Sprintf("azure-storage-container:%+q", v.containerName)
}

// Writable returns true, unless the -readonly flag was on when the
// volume was added.
func (v *AzureBlobVolume) Writable() bool {
	return !v.readonly
}

// Replication returns the replication level of the container, as
// specified by the -azure-storage-replication argument.
func (v *AzureBlobVolume) Replication() int {
	return v.replication
}

// If possible, translate an Azure SDK error to a recognizable error
// like os.ErrNotExist.
func (v *AzureBlobVolume) translateError(err error) error {
	switch {
	case err == nil:
		return err
	case strings.Contains(err.Error(), "Not Found"):
		// "storage: service returned without a response body (404 Not Found)"
		return os.ErrNotExist
	default:
		return err
	}
}

var keepBlockRegexp = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (v *AzureBlobVolume) isKeepBlock(s string) bool {
	return keepBlockRegexp.MatchString(s)
}

// EmptyTrash looks for trashed blocks that exceeded trashLifetime
// and deletes them from the volume.
func (v *AzureBlobVolume) EmptyTrash() {
	var bytesDeleted, bytesInTrash int64
	var blocksDeleted, blocksInTrash int
	params := storage.ListBlobsParameters{Include: "metadata"}

	for {
		resp, err := v.bsClient.ListBlobs(v.containerName, params)
		if err != nil {
			log.Printf("EmptyTrash: ListBlobs: %v", err)
			break
		}
		for _, b := range resp.Blobs {
			// Check if the block is expired
			if b.Metadata["expires_at"] == "" {
				continue
			}

			blocksInTrash++
			bytesInTrash += b.Properties.ContentLength

			expiresAt, err := strconv.ParseInt(b.Metadata["expires_at"], 10, 64)
			if err != nil {
				log.Printf("EmptyTrash: ParseInt(%v): %v", b.Metadata["expires_at"], err)
				continue
			}

			if expiresAt > time.Now().Unix() {
				continue
			}

			err = v.bsClient.DeleteBlob(v.containerName, b.Name, map[string]string{
				"If-Match": b.Properties.Etag,
			})
			if err != nil {
				log.Printf("EmptyTrash: DeleteBlob(%v): %v", b.Name, err)
				continue
			}
			blocksDeleted++
			bytesDeleted += b.Properties.ContentLength
		}
		if resp.NextMarker == "" {
			break
		}
		params.Marker = resp.NextMarker
	}

	log.Printf("EmptyTrash stats for %v: Deleted %v bytes in %v blocks. Remaining in trash: %v bytes in %v blocks.", v.String(), bytesDeleted, blocksDeleted, bytesInTrash-bytesDeleted, blocksInTrash-blocksDeleted)
}
