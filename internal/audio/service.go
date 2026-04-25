package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/ParthSareen/OllamaClaw/internal/config"
)

const (
	defaultTranscribeTimeoutSec = 120
	defaultSynthesizeTimeoutSec = 180
)

type Service struct {
	OllamaHost         string
	TranscriptionModel string
	OllamaBinary       string
	FFmpegBinary       string
	KokoroPython       string
	KokoroVoice        string
	KokoroLangCode     string
	KokoroSpeed        float64
	MaxSpeechChars     int
}

type VoiceFile struct {
	Path            string
	WAVPath         string
	DurationSeconds int
	SpeechText      string
	Cleanup         func()
}

func NewServiceFromConfig(cfg config.Config) *Service {
	return &Service{
		OllamaHost:         strings.TrimSpace(cfg.OllamaHost),
		TranscriptionModel: strings.TrimSpace(cfg.Voice.TranscriptionModel),
		OllamaBinary:       strings.TrimSpace(cfg.Voice.OllamaBinary),
		FFmpegBinary:       strings.TrimSpace(cfg.Voice.FFmpegBinary),
		KokoroPython:       strings.TrimSpace(cfg.Voice.KokoroPython),
		KokoroVoice:        strings.TrimSpace(cfg.Voice.KokoroVoice),
		KokoroLangCode:     strings.TrimSpace(cfg.Voice.KokoroLangCode),
		KokoroSpeed:        cfg.Voice.KokoroSpeed,
		MaxSpeechChars:     cfg.Voice.MaxSpeechChars,
	}
}

func (s *Service) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if strings.TrimSpace(audioPath) == "" {
		return "", errors.New("audio path is required")
	}
	model := defaultString(s.TranscriptionModel, "gemma4:e2b")
	ollamaBin := defaultString(s.OllamaBinary, "ollama")

	wavPath, cleanup, err := s.convertToWAV(ctx, audioPath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	prompt := strings.Join([]string{
		"Transcribe the speech in this audio. Output only a clean, punctuated transcript.",
		"",
		"Context:",
		"- This is a short user message for OllamaClaw, a private agent named Edith.",
		"- The audio may come from a local Mac push-to-talk hotkey harness or a Telegram voice note.",
		"- The speaker often talks about technical work: coding, terminals, logs, repos, GitHub, Go, Python, TypeScript, Ollama, Gemma, Kokoro, ffmpeg, Hammerspoon, Telegram, models, prompts, tests, builds, and local automation.",
		"- Prefer plausible technical terms over unrelated everyday words when the audio is ambiguous.",
		"",
		"Rules:",
		"- Output only what the speaker said.",
		"- Use normal sentence capitalization and punctuation: periods, commas, question marks, apostrophes, and short line breaks where they make the transcript easier to read.",
		"- Add punctuation from the speaker's phrasing and pauses, but do not change the words or make the message more formal.",
		"- Do not answer, summarize, add labels, use JSON, include timestamps, or add commentary.",
		"- Do not include thinking or tool output.",
		"- Do not append the word false or any boolean value unless the speaker clearly said it.",
		"- If speech is unclear, make the best concise transcription rather than inventing extra words.",
	}, "\n")
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(defaultTranscribeTimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, ollamaBin, "run", model, "--think", "false", "--hidethinking", wavPath, prompt)
	if strings.TrimSpace(s.OllamaHost) != "" {
		cmd.Env = append(os.Environ(), "OLLAMA_HOST="+strings.TrimSpace(s.OllamaHost))
	}
	out, err := cmd.CombinedOutput()
	if cmdCtx.Err() != nil {
		return "", cmdCtx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("ollama audio transcription failed: %w: %s", err, strings.TrimSpace(cleanTerminalOutput(string(out))))
	}
	transcript := cleanTranscriptOutput(string(out))
	if transcript == "" {
		return "", errors.New("ollama audio transcription returned an empty transcript")
	}
	return transcript, nil
}

func (s *Service) Synthesize(ctx context.Context, text string) (VoiceFile, error) {
	speechText := PrepareSpeechText(text, s.MaxSpeechChars)
	if speechText == "" {
		speechText = "I finished, but I do not have a spoken summary."
	}
	pythonBin := defaultString(s.KokoroPython, defaultKokoroPython())
	voice := defaultString(s.KokoroVoice, "af_heart")
	langCode := defaultString(s.KokoroLangCode, "a")
	ffmpegBin := defaultString(s.FFmpegBinary, "ffmpeg")
	speed := s.KokoroSpeed
	if speed <= 0 {
		speed = 1.0
	}

	dir, err := os.MkdirTemp("", "ollamaclaw-voice-*")
	if err != nil {
		return VoiceFile{}, fmt.Errorf("create voice temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	textPath := filepath.Join(dir, "speech.txt")
	wavPath := filepath.Join(dir, "speech.wav")
	oggPath := filepath.Join(dir, "speech.ogg")
	if err := os.WriteFile(textPath, []byte(speechText), 0o600); err != nil {
		cleanup()
		return VoiceFile{}, fmt.Errorf("write speech text: %w", err)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(defaultSynthesizeTimeoutSec)*time.Second)
	defer cancel()
	out, err := runKokoroPython(cmdCtx, pythonBin, textPath, wavPath, langCode, voice, speed)
	if cmdCtx.Err() != nil {
		cleanup()
		return VoiceFile{}, cmdCtx.Err()
	}
	if err != nil {
		cleanup()
		return VoiceFile{}, fmt.Errorf("kokoro synthesis failed: %w: %s", err, strings.TrimSpace(cleanTerminalOutput(string(out))))
	}
	duration, err := parseKokoroDuration(out)
	if err != nil {
		cleanup()
		return VoiceFile{}, err
	}
	if err := encodeOpus(ctx, ffmpegBin, wavPath, oggPath); err != nil {
		cleanup()
		return VoiceFile{}, err
	}
	return VoiceFile{
		Path:            oggPath,
		WAVPath:         wavPath,
		DurationSeconds: int(math.Ceil(duration)),
		SpeechText:      speechText,
		Cleanup:         cleanup,
	}, nil
}

func (s *Service) Speak(ctx context.Context, text string) error {
	voice, err := s.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	defer voice.Cleanup()
	return s.PlayWAV(ctx, voice.WAVPath)
}

func (s *Service) PlayWAV(ctx context.Context, wavPath string) error {
	wavPath = strings.TrimSpace(wavPath)
	if wavPath == "" {
		return errors.New("wav path is required")
	}
	cmd := exec.CommandContext(ctx, "afplay", wavPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("play generated voice: %w: %s", err, strings.TrimSpace(cleanTerminalOutput(string(out))))
	}
	return nil
}

func (s *Service) convertToWAV(ctx context.Context, inputPath string) (string, func(), error) {
	ffmpegBin := defaultString(s.FFmpegBinary, "ffmpeg")
	dir, err := os.MkdirTemp("", "ollamaclaw-transcribe-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create transcription temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	wavPath := filepath.Join(dir, "input.wav")
	cmd := exec.CommandContext(ctx, ffmpegBin, "-y", "-hide_banner", "-loglevel", "error", "-i", inputPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wavPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("convert audio to wav: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return wavPath, cleanup, nil
}

func encodeOpus(ctx context.Context, ffmpegBin, wavPath, oggPath string) error {
	cmd := exec.CommandContext(ctx, ffmpegBin, "-y", "-hide_banner", "-loglevel", "error", "-i", wavPath, "-c:a", "libopus", "-b:a", "32k", "-vbr", "on", oggPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("encode opus voice: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runKokoroPython(ctx context.Context, pythonBin, textPath, wavPath, langCode, voice string, speed float64) ([]byte, error) {
	args := []string{"-c", kokoroPythonProgram, textPath, wavPath, langCode, voice, fmt.Sprintf("%g", speed)}
	out, err := runPythonCommand(ctx, pythonBin, args...)
	if err == nil || runtime.GOOS != "darwin" || !strings.Contains(string(out), "incompatible architecture") {
		return out, err
	}
	archArgs := append([]string{"-arm64", pythonBin}, args...)
	return runPythonCommand(ctx, "/usr/bin/arch", archArgs...)
}

func runPythonCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "PYTORCH_ENABLE_MPS_FALLBACK=1")
	return cmd.CombinedOutput()
}

func PrepareSpeechText(raw string, maxChars int) string {
	text := stripCodeBlocks(raw)
	text = markdownLinkRE.ReplaceAllString(text, "$1")
	text = inlineCodeRE.ReplaceAllString(text, "$1")
	text = markdownMarkerRE.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "#", "")
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimSpace(text)
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}
	cut := maxChars
	for i := min(len(text), maxChars); i >= max(0, maxChars-240); i-- {
		switch text[i-1] {
		case '.', '!', '?':
			cut = i
			i = -1
		}
	}
	text = strings.TrimSpace(text[:cut])
	if text == "" {
		return "I wrote a longer response. Full details are in the text reply."
	}
	return text + " Full details are in the text reply."
}

func stripCodeBlocks(raw string) string {
	hadCode := false
	text := codeFenceRE.ReplaceAllStringFunc(raw, func(string) string {
		hadCode = true
		return " I included a code block in the text reply. "
	})
	if hadCode {
		return text
	}
	return raw
}

func cleanTranscriptOutput(raw string) string {
	text := cleanTerminalOutput(raw)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "added audio ") ||
			strings.HasPrefix(lower, "thinking") ||
			strings.Contains(lower, "done thinking") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func cleanTerminalOutput(raw string) string {
	text := ansiRE.ReplaceAllString(raw, "")
	text = brailleRE.ReplaceAllString(text, "")
	var b strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func parseKokoroDuration(out []byte) (float64, error) {
	clean := cleanTerminalOutput(string(out))
	start := strings.LastIndex(clean, "{")
	if start < 0 {
		return 0, fmt.Errorf("kokoro synthesis did not report metadata: %s", clean)
	}
	var meta struct {
		DurationSeconds float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal([]byte(clean[start:]), &meta); err != nil {
		return 0, fmt.Errorf("decode kokoro metadata: %w: %s", err, clean)
	}
	if meta.DurationSeconds <= 0 {
		return 0, fmt.Errorf("kokoro reported invalid duration: %.3f", meta.DurationSeconds)
	}
	return meta.DurationSeconds, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func defaultKokoroPython() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "python3"
	}
	return filepath.Join(home, ".ollamaclaw", "kokoro-test", "venv", "bin", "python")
}

var (
	ansiRE           = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	brailleRE        = regexp.MustCompile(`[\x{2800}-\x{28FF}]`)
	codeFenceRE      = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRE     = regexp.MustCompile("`([^`]+)`")
	markdownLinkRE   = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	markdownMarkerRE = regexp.MustCompile(`[*_~>]+`)
)

const kokoroPythonProgram = `
import json
import sys
import time

import numpy as np
import soundfile as sf
from kokoro import KPipeline

text_path, out_path, lang_code, voice, speed_raw = sys.argv[1:6]
speed = float(speed_raw)
text = open(text_path, "r", encoding="utf-8").read().strip()
if not text:
    raise RuntimeError("empty speech text")

started = time.perf_counter()
pipeline = KPipeline(lang_code=lang_code, repo_id="hexgrad/Kokoro-82M")
chunks = []
for _, _, audio in pipeline(text, voice=voice, speed=speed):
    chunks.append(np.asarray(audio, dtype=np.float32))
if not chunks:
    raise RuntimeError("kokoro returned no audio")

audio = np.concatenate(chunks)
sample_rate = 24000
sf.write(out_path, audio, sample_rate)
print(json.dumps({
    "sample_rate": sample_rate,
    "samples": int(len(audio)),
    "duration_seconds": float(len(audio) / sample_rate),
    "elapsed_seconds": float(time.perf_counter() - started),
}))
`

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
