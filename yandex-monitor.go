package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	YandexDiskAPIURL          = "https://cloud-api.yandex.net/v1/disk/resources/upload"
	YandexStatusPath          = "app:/status.txt"
	YandexFailureStatusPeriod = 24 * time.Hour
)

type yandexUploadResponse struct {
	Method      string `json:"method"`
	Href        string `json:"href"`
	Templated   *bool  `json:"templated"`
	OperationID string `json:"operation_id"`
}

func newYandexUploadURLRequest(clientToken string) (*http.Request, error) {
	query := url.Values{}
	query.Set("path", YandexStatusPath)
	query.Set("overwrite", "true")
	request, err := http.NewRequest(
		http.MethodGet,
		YandexDiskAPIURL+"?"+query.Encode(),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating Yandex Disk upload URL request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "OAuth "+clientToken)

	return request, nil
}

func getYandexUploadURL(client *http.Client, uploadURLRequest *http.Request) (string, error) {
	response, err := client.Do(uploadURLRequest)
	if err != nil {
		return "", fmt.Errorf("error requesting Yandex Disk upload URL: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("Yandex Disk upload URL request returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	var uploadResponse yandexUploadResponse
	if err := json.NewDecoder(response.Body).Decode(&uploadResponse); err != nil {
		return "", fmt.Errorf("error parsing Yandex Disk upload URL response: %w", err)
	}
	if uploadResponse.Method != http.MethodPut {
		log.Printf(
			"Warning: Yandex Disk upload URL response method is %q, expected %q",
			uploadResponse.Method,
			http.MethodPut,
		)
	}
	if uploadResponse.Templated == nil {
		log.Println("Warning: Yandex Disk upload URL response does not contain templated; expected false")
	} else if *uploadResponse.Templated {
		log.Println("Warning: Yandex Disk upload URL response has templated=true; expected false")
	}
	if uploadResponse.OperationID == "" {
		log.Println("Warning: Yandex Disk upload URL response has no operation_id")
	}
	if uploadResponse.Href == "" {
		log.Println("Warning: Yandex Disk upload URL response has no href")
		return "", fmt.Errorf("Yandex Disk upload URL response has no href")
	}

	return uploadResponse.Href, nil
}

func uploadToYandexDisk(
	client *http.Client,
	uploadURLRequest *http.Request,
	message string,
) error {
	uploadURL, err := getYandexUploadURL(client, uploadURLRequest)
	if err != nil {
		return fmt.Errorf("error preparing Yandex Disk upload URL: %w", err)
	}

	putRequest, err := http.NewRequest(
		http.MethodPut,
		uploadURL,
		bytes.NewBufferString(message),
	)
	if err != nil {
		return fmt.Errorf("error creating Yandex Disk file upload request: %w", err)
	}
	putRequest.Header.Set("Content-Type", "text/plain; charset=utf-8")

	putResponse, err := client.Do(putRequest)
	if err != nil {
		return fmt.Errorf("error uploading file to Yandex Disk: %w", err)
	}
	defer putResponse.Body.Close()

	if putResponse.StatusCode != http.StatusCreated && putResponse.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(putResponse.Body, 4096))
		return fmt.Errorf("Yandex Disk file upload returned %s: %s", putResponse.Status, strings.TrimSpace(string(body)))
	}

	return nil
}

func runYandexMonitor(clientToken string) {
	client := &http.Client{Timeout: time.Minute}
	uploadURLRequest, err := newYandexUploadURLRequest(clientToken)
	if err != nil {
		log.Printf("Error creating Yandex Disk upload URL request: %v", err)
		return
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	var failureTime time.Time
	var failureStatusUntil time.Time
	var errMsg string

	for {
		now := time.Now()
		statusText := fmt.Sprintf(
			"Связь есть %s\n\nСообщение будет обновляться каждые 10 минут, пока есть связь.",
			now.Format("02.01.2006 15:04 MST"),
		)
		if !failureTime.IsZero() && now.Before(failureStatusUntil) {
			statusText = fmt.Sprintf(
				"%s\n\nПодключение обрывалось %s\n%s", statusText,
				failureTime.Format(time.RFC1123), errMsg,
			)
		}

		if err := uploadToYandexDisk(client, uploadURLRequest, statusText); err == nil {
			if !failureTime.IsZero() && time.Now().After(failureStatusUntil) {
				failureTime = time.Time{}
			}
			<-ticker.C
			continue
		} else {
			errMsg = fmt.Sprintf("Error uploading Yandex Disk status: %v", err)
		}
		log.Print(errMsg)

		if failureTime.IsZero() {
			failureTime = now
			failureStatusUntil = now.Add(YandexFailureStatusPeriod)
		}

		<-ticker.C
	}
}
