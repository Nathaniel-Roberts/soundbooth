package main

import "testing"

func TestTranscribeLineParsing(t *testing.T) {
	if stageOf("2026-08-17 - whispermlx.asr - INFO - Loading MLX Whisper model: x") != 0 {
		t.Error("load model stage")
	}
	if stageOf("whispermlx.vads.pyannote - INFO - Performing voice activity detection using Pyannote...") != 1 {
		t.Error("vad stage")
	}
	if stageOf("Transcribing:  42%|████      | 5/12 [00:03<00:04,  1.49seg/s]") != 2 {
		t.Error("transcribe stage")
	}
	if stageOf("INFO - Performing alignment...") != 3 {
		t.Error("align stage")
	}
	if stageOf("INFO - Performing diarization...") != 4 {
		t.Error("diarize stage")
	}
	if pm := pctRe.FindStringSubmatch("Transcribing:  42%|██| 5/12"); pm == nil || pm[1] != "42" {
		t.Errorf("pct parse: %v", pm)
	}
	if sm := segLineRe.FindStringSubmatch("[2.174 --> 3.507] Testing one, two, three."); sm == nil || sm[3] != "Testing one, two, three." {
		t.Errorf("segment parse: %v", sm)
	}
}
