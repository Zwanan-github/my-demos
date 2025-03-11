package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var minioClient *minio.Client
var bucketName = "gin-upload-bucket" // You can change this
var uploadProgress = make(map[string]*Progress)
var progressMutex sync.Mutex

type Progress struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	TotalSize    int64     `json:"totalSize"`
	UploadedSize int64     `json:"uploadedSize"`
	StartTime    time.Time `json:"startTime"`
}

type ProgressReader struct {
	io.Reader
	progress *Progress
	current  int64
}

func (pr *ProgressReader) Read(p []byte) (n int, err error) {
	n, err = pr.Reader.Read(p)
	if n > 0 {
		pr.current += int64(n)
		pr.progress.UploadedSize = pr.current
		progressMutex.Lock()
		uploadProgress[pr.progress.ID] = pr.progress // Update progress in map
		progressMutex.Unlock()
	}
	return n, err
}

func main() {
	endpoint := "minio-api.zwanan.top"                            // Replace with your Minio endpoint if needed
	accessKeyID := "OXT6zVCf3cTw7VUtY8xG"                         // Replace with your Minio access key
	secretAccessKey := "cXTE9xnJZwt0m4tYoPlf8Y1jZRxAIqwKTseSQzGi" // Replace with your Minio secret key
	useSSL := false

	// Initialize Minio client
	var err error
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		log.Fatalln(err)
	}

	// Create bucket if not exists
	ctx := context.Background()
	exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
	if errBucketExists != nil {
		log.Fatalln(errBucketExists)
	}
	if !exists {
		err := minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
		if err != nil {
			log.Fatalln(err)
		}
		log.Printf("Bucket '%s' created successfully\n", bucketName)
	} else {
		log.Printf("Bucket '%s' already exists\n", bucketName)
	}

	r := gin.Default()

	r.POST("/upload", uploadFile)
	r.GET("/download/:filename", downloadFile)
	r.GET("/progress/:id", getProgress)

	r.Run(":8080")
}

func uploadFile(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.String(http.StatusBadRequest, "Bad request")
		return
	}

	fileSize := header.Size
	filename := header.Filename
	uploadID := generateUploadID()

	progress := &Progress{
		ID:           uploadID,
		Filename:     filename,
		TotalSize:    fileSize,
		UploadedSize: 0,
		StartTime:    time.Now(),
	}
	progressMutex.Lock()
	uploadProgress[uploadID] = progress
	progressMutex.Unlock()

	c.JSON(http.StatusOK, gin.H{"uploadId": uploadID, "message": "Upload started"}) // Return upload ID immediately

	// Start a goroutine to handle the actual file upload
	go func() {
		defer file.Close()
		progressReader := &ProgressReader{
			Reader:   file,
			progress: progress,
			current:  0,
		}

		ctx := context.Background()
		_, err := minioClient.PutObject(ctx, bucketName, filename, progressReader, fileSize, minio.PutObjectOptions{ContentType: header.Header.Get("Content-Type")})
		if err != nil {
			log.Println(err)
			progressMutex.Lock()
			delete(uploadProgress, uploadID) // Remove progress on failure
			progressMutex.Unlock()
			return // Or handle error in a way that makes sense for your application
		}
		log.Printf("File '%s' uploaded successfully with ID '%s'\n", filename, uploadID)
	}()
}

func downloadFile(c *gin.Context) {
	filename := c.Param("filename")

	ctx := context.Background()
	object, err := minioClient.GetObject(ctx, bucketName, filename, minio.GetObjectOptions{})
	if err != nil {
		c.String(http.StatusInternalServerError, "File download failed")
		return
	}
	defer object.Close()

	fileInfo, err := object.Stat()
	if err != nil {
		c.String(http.StatusInternalServerError, "File download failed")
		return
	}

	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", fileInfo.ContentType)
	c.Header("Content-Length", strconv.FormatInt(fileInfo.Size, 10)) // Corrected to use strconv.FormatInt

	_, err = io.Copy(c.Writer, object)
	if err != nil {
		c.String(http.StatusInternalServerError, "File download failed")
		return
	}
}

func getProgress(c *gin.Context) {
	id := c.Param("id")
	progressMutex.Lock()
	_, exists := uploadProgress[id]
	progressMutex.Unlock()
	if !exists {
		c.String(http.StatusNotFound, "Progress not found")
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	clientChan := make(chan string)
	done := make(chan bool)
	defer close(clientChan)
	defer close(done)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Println("Panic in progress goroutine:", r)
			}
		}()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				progressMutex.Lock()
				currentProgress, exists := uploadProgress[id]
				progressMutex.Unlock()
				if !exists {
					return
				}

				progressJSON, err := json.Marshal(currentProgress)
				if err != nil {
					log.Println("Error marshaling progress:", err)
					return
				}

				select {
				case clientChan <- fmt.Sprintf("data: %s\n\n", progressJSON):
				default:
					return
				}

				if currentProgress.UploadedSize == currentProgress.TotalSize {
					return
				}
			}
		}
	}()

	c.Stream(func(w io.Writer) bool {
		for {
			select {
			case msg := <-clientChan:
				c.SSEvent("message", msg)
				return true
			case <-c.Request.Context().Done():
				done <- true
				return false
			}
		}
	})
}

func generateUploadID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()) // Simple ID generation, consider UUID for production
}
