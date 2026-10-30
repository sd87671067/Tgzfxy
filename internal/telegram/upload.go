package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"
)

// Upload streams from disk/network to avoid retaining media in the Go heap.
func (c *Client) Upload(ctx context.Context, chat int64, method, field, name string, reader io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	writer := multipart.NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		err := writer.WriteField("chat_id", fmt.Sprint(chat))
		if err == nil {
			var part io.Writer
			part, err = writer.CreateFormFile(field, filepath.Base(name))
			if err == nil {
				_, err = io.Copy(part, reader)
			}
		}
		if err == nil {
			err = writer.Close()
		}
		pw.CloseWithError(err)
		done <- err
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+method, pr)
	if err != nil {
		pr.CloseWithError(err)
		<-done
		return errors.New("invalid Telegram upload endpoint")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	client := &http.Client{Transport: c.HTTP.Transport, Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		<-done
		return errors.New("Telegram media upload failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK   bool `json:"ok"`
		Code int  `json:"error_code"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	pr.Close()
	copyErr := <-done
	if err != nil {
		return err
	}
	if !result.OK {
		return &APIError{Code: result.Code}
	}
	return copyErr
}
