package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// limit of 1GB
	const maxSize = 1 << 30

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoMetaData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Unable to get video metadata", err)
		return
	}
	if videoMetaData.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid file type, only mp4 allowed. You uploaded %s", mediaType), nil)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	defer os.Remove("tubely-upload.mp4")
	defer tmpFile.Close()

	io.Copy(tmpFile, file)

	tmpFile.Seek(0, io.SeekStart)

	aspectRatio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get aspect ratio", err)
		return
	}

	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	randBytes := make([]byte, 32)
	rand.Read(randBytes)
	uniqueName := base64.RawURLEncoding.EncodeToString(randBytes)
	fileName := fmt.Sprintf("%s/%s.%s", prefix, uniqueName, "mp4")

	processedVideoPath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process video for fast start", err)
		return
	}

	processedVideoFile, err := os.Open(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open processed video", err)
		return
	}
	defer os.Remove(processedVideoPath)
	defer processedVideoFile.Close()

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileName,
		Body:        processedVideoFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to put object to s3", err)
		return
	}

	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileName)
	videoMetaData.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoMetaData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video metadata", err)
		return
	}
	respondWithJSON(w, http.StatusOK, videoMetaData)
}

func getVideoAspectRatio(filePath string) (string, error) {
	ffprobe := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var b bytes.Buffer
	ffprobe.Stdout = &b
	err := ffprobe.Run()
	if err != nil {
		return "", err
	}
	vData := VideoData{}
	json.Unmarshal(b.Bytes(), &vData)
	height := float64(vData.Streams[0].Height)
	width := float64(vData.Streams[0].Width)
	ratio := width / height
	if ratio >= 1.777 && ratio <= 1.778 {
		return "16:9", nil
	} else if ratio >= 0.562 && ratio <= 0.563 {
		return "9:16", nil

	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	newPath := filePath + ".processing"
	ffmpeg := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newPath)
	err := ffmpeg.Run()
	if err != nil {
		return "", err
	}
	return newPath, err
}

type VideoData struct {
	Streams []struct {
		Index              int    `json:"index"`
		CodecName          string `json:"codec_name,omitempty"`
		CodecLongName      string `json:"codec_long_name,omitempty"`
		Profile            string `json:"profile,omitempty"`
		CodecType          string `json:"codec_type"`
		CodecTagString     string `json:"codec_tag_string"`
		CodecTag           string `json:"codec_tag"`
		Width              int    `json:"width,omitempty"`
		Height             int    `json:"height,omitempty"`
		CodedWidth         int    `json:"coded_width,omitempty"`
		CodedHeight        int    `json:"coded_height,omitempty"`
		ClosedCaptions     int    `json:"closed_captions,omitempty"`
		HasBFrames         int    `json:"has_b_frames,omitempty"`
		SampleAspectRatio  string `json:"sample_aspect_ratio,omitempty"`
		DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
		PixFmt             string `json:"pix_fmt,omitempty"`
		Level              int    `json:"level,omitempty"`
		ColorRange         string `json:"color_range,omitempty"`
		ColorSpace         string `json:"color_space,omitempty"`
		ColorTransfer      string `json:"color_transfer,omitempty"`
		ColorPrimaries     string `json:"color_primaries,omitempty"`
		ChromaLocation     string `json:"chroma_location,omitempty"`
		Refs               int    `json:"refs,omitempty"`
		IsAvc              string `json:"is_avc,omitempty"`
		NalLengthSize      string `json:"nal_length_size,omitempty"`
		RFrameRate         string `json:"r_frame_rate"`
		AvgFrameRate       string `json:"avg_frame_rate"`
		TimeBase           string `json:"time_base"`
		StartPts           int    `json:"start_pts"`
		StartTime          string `json:"start_time"`
		DurationTs         int    `json:"duration_ts"`
		Duration           string `json:"duration"`
		BitRate            string `json:"bit_rate,omitempty"`
		BitsPerRawSample   string `json:"bits_per_raw_sample,omitempty"`
		NbFrames           string `json:"nb_frames"`
		Disposition        struct {
			Default         int `json:"default"`
			Dub             int `json:"dub"`
			Original        int `json:"original"`
			Comment         int `json:"comment"`
			Lyrics          int `json:"lyrics"`
			Karaoke         int `json:"karaoke"`
			Forced          int `json:"forced"`
			HearingImpaired int `json:"hearing_impaired"`
			VisualImpaired  int `json:"visual_impaired"`
			CleanEffects    int `json:"clean_effects"`
			AttachedPic     int `json:"attached_pic"`
			TimedThumbnails int `json:"timed_thumbnails"`
		} `json:"disposition"`
		Tags struct {
			Language    string `json:"language"`
			HandlerName string `json:"handler_name"`
			VendorID    string `json:"vendor_id"`
			Encoder     string `json:"encoder"`
			Timecode    string `json:"timecode"`
		} `json:"tags,omitempty"`
		SampleFmt     string `json:"sample_fmt,omitempty"`
		SampleRate    string `json:"sample_rate,omitempty"`
		Channels      int    `json:"channels,omitempty"`
		ChannelLayout string `json:"channel_layout,omitempty"`
		BitsPerSample int    `json:"bits_per_sample,omitempty"`
		Tags0         struct {
			Language    string `json:"language"`
			HandlerName string `json:"handler_name"`
			VendorID    string `json:"vendor_id"`
		} `json:"tags,omitempty"`
		Tags1 struct {
			Language    string `json:"language"`
			HandlerName string `json:"handler_name"`
			Timecode    string `json:"timecode"`
		} `json:"tags,omitempty"`
	} `json:"streams"`
}
