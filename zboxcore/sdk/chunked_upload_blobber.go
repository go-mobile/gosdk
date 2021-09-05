package sdk

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	thrown "github.com/0chain/errors"
	"github.com/0chain/gosdk/core/common"
	"github.com/0chain/gosdk/core/encryption"
	"github.com/0chain/gosdk/zboxcore/allocationchange"
	"github.com/0chain/gosdk/zboxcore/blockchain"
	"github.com/0chain/gosdk/zboxcore/client"
	"github.com/0chain/gosdk/zboxcore/fileref"
	"github.com/0chain/gosdk/zboxcore/logger"
	"github.com/0chain/gosdk/zboxcore/marker"
	"github.com/0chain/gosdk/zboxcore/zboxutil"
	"github.com/gofrs/flock"
)

// ChunkedUploadBobbler client of blobber's upload
type ChunkedUploadBobbler struct {
	flock    *flock.Flock
	blobber  *blockchain.StorageNode
	fileRef  *fileref.FileRef
	progress *UploadBlobberStatus

	commitChanges []allocationchange.AllocationChange
	commitResult  *CommitResult
}

// processHash update ChallengeHash and ContentHash
func (sb *ChunkedUploadBobbler) processHash(fileBytes []byte, chunkHash string, chunkIndex int) {
	sb.progress.ChallengeHasher.Write(fileBytes, chunkIndex)
	sb.progress.ContentHasher.AddLeaf(chunkHash, chunkIndex)
}

func (sb *ChunkedUploadBobbler) processUpload(su *ChunkedUpload, chunkIndex int, fileBytes, thumbnailBytes []byte, isFinal bool, wg *sync.WaitGroup) {
	defer wg.Done()

	if len(fileBytes) == 0 {
		//fixed fileRef in last chunk on stream. io.EOF with nil bytes
		if isFinal {
			sb.fileRef.ChunkSize = su.chunkSize
			sb.fileRef.Size = su.shardUploadedSize
			sb.fileRef.Path = su.fileMeta.RemotePath
			sb.fileRef.ActualFileHash = su.fileMeta.ActualHash
			sb.fileRef.ActualFileSize = su.fileMeta.ActualSize

			sb.fileRef.EncryptedKey = su.fileEncscheme.GetEncryptedKey()
			sb.fileRef.CalculateHash()
		}

		return
	}

	body := new(bytes.Buffer)

	formData := UploadFormData{
		ConnectionID: su.progress.ConnectionID,
		Filename:     su.fileMeta.RemoteName,
		Path:         su.fileMeta.RemotePath,

		ActualSize: su.fileMeta.ActualSize,

		ActualThumbHash: su.fileMeta.ActualThumbnailHash,
		ActualThumbSize: su.fileMeta.ActualThumbnailSize,

		MimeType:   su.fileMeta.MimeType,
		Attributes: su.fileMeta.Attributes,

		IsFinal:      isFinal,
		ChunkSize:    int64(su.chunkSize),
		ChunkIndex:   chunkIndex,
		UploadOffset: int64(su.chunkSize * chunkIndex),
	}

	formWriter := multipart.NewWriter(body)

	uploadFile, err := formWriter.CreateFormFile("uploadFile", formData.Filename)
	if err != nil {
		logger.Logger.Error("[upload] Create form on field [uploadFile] failed: ", err)
		return
	}

	chunkHashWriter := sha1.New()
	chunkWriters := io.MultiWriter(uploadFile, chunkHashWriter)

	chunkWriters.Write(fileBytes)

	formData.ChunkHash = hex.EncodeToString(chunkHashWriter.Sum(nil))
	formData.ContentHash = formData.ChunkHash

	sb.processHash(fileBytes, formData.ChunkHash, chunkIndex)

	if isFinal {

		//fixed shard data's info in last chunk for stream
		formData.MerkleRoot = sb.progress.getChallengeHash()
		formData.ContentHash = sb.progress.getContentHash()

		//fixed original file's info in last chunk for stream
		formData.ActualHash = su.fileMeta.ActualHash
		formData.ActualSize = su.fileMeta.ActualSize

	}

	thumbnailSize := len(thumbnailBytes)
	if thumbnailSize > 0 {

		uploadThumbnailFile, err := formWriter.CreateFormFile("uploadThumbnailFile", su.fileMeta.RemoteName+".thumb")
		if err != nil {
			logger.Logger.Error("[upload] Create form on field [uploadThumbnailFile] failed: ", err)
			return
		}

		thumbnailHash := sha1.New()
		thumbnailWriters := io.MultiWriter(uploadThumbnailFile, thumbnailHash)
		thumbnailWriters.Write(thumbnailBytes)

		formData.ActualThumbSize = su.fileMeta.ActualThumbnailSize
		formData.ThumbnailContentHash = hex.EncodeToString(thumbnailHash.Sum(nil))

	}

	if su.encryptOnUpload {
		formData.EncryptedKey = su.fileEncscheme.GetEncryptedKey()

		sb.fileRef.EncryptedKey = formData.EncryptedKey
	}
	_ = formWriter.WriteField("connection_id", su.progress.ConnectionID)

	metaData, err := json.Marshal(formData)

	_ = formWriter.WriteField("uploadMeta", string(metaData))

	formWriter.Close()
	httpreq, _ := zboxutil.NewUploadRequestWithMethod(sb.blobber.Baseurl, su.allocationObj.Tx, body, su.httpMethod)

	httpreq.Header.Add("Content-Type", formWriter.FormDataContentType())

	//TODO: retry http
	err = zboxutil.HttpDo(su.allocationObj.ctx, su.allocationObj.ctxCancelF, httpreq, func(resp *http.Response, err error) error {
		if err != nil {
			logger.Logger.Error("Upload : ", err)
			//req.err = err
			return err
		}
		defer resp.Body.Close()

		respbody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			logger.Logger.Error("Error: Resp ", err)
			//req.err = err
			return err
		}
		if resp.StatusCode != http.StatusOK {
			logger.Logger.Error(sb.blobber.Baseurl, " Upload error response: ", resp.StatusCode, string(respbody))
			//req.err = fmt.Errorf(string(respbody))
			return err
		}
		var r uploadResult
		err = json.Unmarshal(respbody, &r)
		if err != nil {
			logger.Logger.Error(sb.blobber.Baseurl, " Upload response parse error: ", err)
			//req.err = err
			return err
		}
		if r.Filename != formData.Filename || r.Hash != formData.ChunkHash {
			err = fmt.Errorf("%s Unexpected upload response data %s %s %s", sb.blobber.Baseurl, formData.Filename, formData.ChunkHash, string(respbody))
			logger.Logger.Error(err)
			//req.err = err
			return err
		}
		//req.consensus++
		logger.Logger.Info(sb.blobber.Baseurl, formData.Path, " uploaded")

		su.Done()

		return nil
	})

	//fixed fileRef
	if err == nil {

		//fixed thumbnail info in first chunk if it has thumbnail
		if len(thumbnailBytes) > 0 {

			sb.fileRef.ThumbnailSize = int64(len(thumbnailBytes))
			sb.fileRef.ThumbnailHash = formData.ThumbnailContentHash

			sb.fileRef.ActualThumbnailSize = su.fileMeta.ActualThumbnailSize
			sb.fileRef.ActualThumbnailHash = su.fileMeta.ActualThumbnailHash
		}

		//fixed fileRef in last chunk on stream
		if isFinal {
			sb.fileRef.MerkleRoot = formData.MerkleRoot
			sb.fileRef.ContentHash = formData.ContentHash

			sb.fileRef.ChunkSize = su.chunkSize
			sb.fileRef.Size = su.shardUploadedSize
			sb.fileRef.Path = formData.Path
			sb.fileRef.ActualFileHash = formData.ActualHash
			sb.fileRef.ActualFileSize = formData.ActualSize

			sb.fileRef.EncryptedKey = formData.EncryptedKey
			sb.fileRef.CalculateHash()
		}
	}

}

func (sb *ChunkedUploadBobbler) processCommit(su *ChunkedUpload, wg *sync.WaitGroup) error {
	defer wg.Done()

	err := sb.flock.Lock()
	if err != nil {
		return err
	}
	defer sb.flock.Unlock()

	rootRef, latestWM, size, err := sb.processWriteMarker(su)

	if err != nil {
		return err
	}

	wm := &marker.WriteMarker{}
	timestamp := int64(common.Now())
	wm.AllocationRoot = encryption.Hash(rootRef.Hash + ":" + strconv.FormatInt(timestamp, 10))
	if latestWM != nil {
		wm.PreviousAllocationRoot = latestWM.AllocationRoot
	} else {
		wm.PreviousAllocationRoot = ""
	}

	wm.AllocationID = su.allocationObj.ID
	wm.Size = size
	wm.BlobberID = sb.blobber.ID

	wm.Timestamp = timestamp
	wm.ClientID = client.GetClientID()
	err = wm.Sign()
	if err != nil {
		logger.Logger.Error("Signing writemarker failed: ", err)
		return err
	}
	body := new(bytes.Buffer)
	formWriter := multipart.NewWriter(body)
	wmData, err := json.Marshal(wm)
	if err != nil {
		logger.Logger.Error("Creating writemarker failed: ", err)
		return err
	}
	formWriter.WriteField("connection_id", su.progress.ConnectionID)
	formWriter.WriteField("write_marker", string(wmData))

	formWriter.Close()

	httpreq, err := zboxutil.NewCommitRequest(sb.blobber.Baseurl, su.allocationObj.Tx, body)
	if err != nil {
		logger.Logger.Error("Error creating commit req: ", err)
		return err
	}
	httpreq.Header.Add("Content-Type", formWriter.FormDataContentType())

	logger.Logger.Info("Committing to blobber." + sb.blobber.Baseurl)

	//for retries := 0; retries < 3; retries++ {
	ctx, cncl := context.WithTimeout(context.Background(), (time.Second * 60))
	err = zboxutil.HttpDo(ctx, cncl, httpreq, func(resp *http.Response, err error) error {
		if err != nil {
			logger.Logger.Error("Commit: ", err)
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			logger.Logger.Info(sb.blobber.Baseurl, su.progress.ConnectionID, " committed")
		} else {
			logger.Logger.Error("Commit response: ", resp.StatusCode)
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			logger.Logger.Error("Response read: ", err)
			return err
		}
		if resp.StatusCode != http.StatusOK {
			logger.Logger.Error(sb.blobber.Baseurl, " Commit response:", string(body))
			return thrown.New("commit_error", string(body))
		}
		return nil
	})

	if err == nil {
		su.Done()
		return nil
	}
	//}

	return nil
}

func (sb *ChunkedUploadBobbler) processWriteMarker(su *ChunkedUpload) (*fileref.Ref, *marker.WriteMarker, int64, error) {
	logger.Logger.Info("received a commit request")
	paths := make([]string, 0)
	for _, change := range sb.commitChanges {
		paths = append(paths, change.GetAffectedPath())
	}
	var req *http.Request
	var lR ReferencePathResult
	req, err := zboxutil.NewReferencePathRequest(sb.blobber.Baseurl, su.allocationObj.Tx, paths)
	if err != nil || len(paths) == 0 {
		logger.Logger.Error("Creating ref path req", err)
		return nil, nil, 0, err
	}
	ctx, cncl := context.WithTimeout(context.Background(), (time.Second * 30))
	err = zboxutil.HttpDo(ctx, cncl, req, func(resp *http.Response, err error) error {
		if err != nil {
			logger.Logger.Error("Ref path error:", err)
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			logger.Logger.Error("Ref path response : ", resp.StatusCode)
		}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			logger.Logger.Error("Ref path: Resp", err)
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Reference path error response: Status: %d - %s ", resp.StatusCode, string(body))
		}

		err = json.Unmarshal(body, &lR)
		if err != nil {
			logger.Logger.Error("Reference path json decode error: ", err)
			return err
		}

		return nil
	})
	//process the commit request for the blobber here
	if err != nil {
		sb.commitResult = ErrorCommitResult(err.Error())

		return nil, nil, 0, err
	}
	rootRef, err := lR.GetDirTree(su.allocationObj.ID)
	if lR.LatestWM != nil {

		rootRef.CalculateHash()
		prevAllocationRoot := encryption.Hash(rootRef.Hash + ":" + strconv.FormatInt(lR.LatestWM.Timestamp, 10))
		// TODO: it is a concurrent change conflict on database.  check concurrent write for allocation
		if prevAllocationRoot != lR.LatestWM.AllocationRoot {
			logger.Logger.Info("Allocation root from latest writemarker mismatch. Expected: " + prevAllocationRoot + " got: " + lR.LatestWM.AllocationRoot)
		}
	}
	if err != nil {
		sb.commitResult = ErrorCommitResult(err.Error())
		return nil, nil, 0, err
	}
	size := int64(0)
	for _, change := range sb.commitChanges {
		err = change.ProcessChange(rootRef)
		if err != nil {
			break
		}
		size += change.GetSize()
	}
	if err != nil {
		sb.commitResult = ErrorCommitResult(err.Error())
		return nil, nil, 0, err
	}

	return rootRef, lR.LatestWM, size, nil
}