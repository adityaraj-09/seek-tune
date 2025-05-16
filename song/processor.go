package song

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"song-recognition/db"
	"song-recognition/shazam"
	"song-recognition/utils"
	"song-recognition/wav"
	"strconv"
	"strings"
)

type SongInput struct {
	SongURL   string `json:"song_url"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	YoutubeID string `json:"youtube_id,omitempty"`
	Duration  string `json:"duration,omitempty"`
}

type ProcessResponse struct {
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	FilePath      string `json:"file_path,omitempty"`
	FingerprintID string `json:"fingerprint_id,omitempty"`
}

func convertToWav(inputPath string) (string, error) {
	outputPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".wav"
	cmd := exec.Command("ffmpeg", "-i", inputPath, "-acodec", "pcm_s16le", "-ar", "44100", "-ac", "2", outputPath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to convert to WAV: %v", err)
	}
	return outputPath, nil
}

func ProcessSongFromURL(input *SongInput) (*ProcessResponse, error) {
	logger := utils.GetLogger()
	ctx := context.Background()

	// Create necessary directories
	err := utils.CreateFolder("tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create tmp directory: %v", err)
	}

	err = utils.CreateFolder("songs")
	if err != nil {
		return nil, fmt.Errorf("failed to create songs directory: %v", err)
	}

	// Download the file
	resp, err := http.Get(input.SongURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download song: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("received non-200 status code: %d", resp.StatusCode)
	}

	// Create a temporary file with the downloaded content (MP3)
	tmpMP3File := filepath.Join("tmp", fmt.Sprintf("%s_%s.mp3", input.Title, input.Artist))
	out, err := os.Create(tmpMP3File)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to save downloaded file: %v", err)
	}

	// Convert MP3 to WAV
	tmpWavFile, err := convertToWav(tmpMP3File)
	if err != nil {
		logger.ErrorContext(ctx, "Error converting to WAV", slog.Any("error", err))
		return nil, fmt.Errorf("error converting to WAV: %v", err)
	}
	defer os.Remove(tmpMP3File) // Clean up the MP3 file

	// Process the WAV file
	wavInfo, err := wav.ReadWavInfo(tmpWavFile)
	if err != nil {
		logger.ErrorContext(ctx, "Error reading wave info", slog.Any("error", err))
		return nil, fmt.Errorf("error reading wave info: %v", err)
	}

	samples, err := wav.WavBytesToSamples(wavInfo.Data)
	if err != nil {
		logger.ErrorContext(ctx, "Error converting to samples", slog.Any("error", err))
		return nil, fmt.Errorf("error converting to samples: %v", err)
	}

	// Generate spectrogram and extract peaks
	spectrogram, err := shazam.Spectrogram(samples, wavInfo.SampleRate)
	if err != nil {
		logger.ErrorContext(ctx, "Error generating spectrogram", slog.Any("error", err))
		return nil, fmt.Errorf("error generating spectrogram: %v", err)
	}

	peaks := shazam.ExtractPeaks(spectrogram, wavInfo.Duration)
	songID := utils.GenerateUniqueID()
	fingerprints := shazam.Fingerprint(peaks, songID)

	// Save fingerprints to database
	dbClient, err := db.NewDBClient()
	if err != nil {
		logger.ErrorContext(ctx, "Error creating DB client", slog.Any("error", err))
		return nil, fmt.Errorf("error creating DB client: %v", err)
	}
	defer dbClient.Close()

	// Register the song first
	registeredSongID, err := dbClient.RegisterSong(input.Title, input.Artist, input.YoutubeID)
	if err != nil {
		logger.ErrorContext(ctx, "Error registering song", slog.Any("error", err))
		return nil, fmt.Errorf("error registering song: %v", err)
	}

	// Store fingerprints
	err = dbClient.StoreFingerprints(fingerprints)
	if err != nil {
		logger.ErrorContext(ctx, "Error storing fingerprints", slog.Any("error", err))
		return nil, fmt.Errorf("error storing fingerprints: %v", err)
	}

	// Move file to songs directory
	finalPath := filepath.Join("songs", fmt.Sprintf("%s_%s.wav", input.Title, input.Artist))
	err = os.Rename(tmpWavFile, finalPath)
	if err != nil {
		logger.ErrorContext(ctx, "Error moving file to songs directory", slog.Any("error", err))
		// Don't return error here as fingerprints are already saved
	}

	return &ProcessResponse{
		Success:       true,
		Message:       "Song processed successfully",
		FilePath:      finalPath,
		FingerprintID: strconv.FormatUint(uint64(registeredSongID), 10),
	}, nil
}

func ProcessSongJSON(jsonInput []byte) (*ProcessResponse, error) {
	var input SongInput
	err := json.Unmarshal(jsonInput, &input)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON input: %v", err)
	}

	if input.SongURL == "" {
		return nil, fmt.Errorf("song_url is required")
	}
	if input.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if input.Artist == "" {
		return nil, fmt.Errorf("artist is required")
	}

	return ProcessSongFromURL(&input)
}
