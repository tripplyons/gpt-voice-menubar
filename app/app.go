package app

import (
	"github.com/go-audio/wav"
	"github.com/go-audio/audio"
	"github.com/orcaman/writerseeker"
	"io"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"bytes"
	"log"
	"github.com/getlantern/systray"
	hook "github.com/robotn/gohook"
	"github.com/gordonklaus/portaudio"
	"fmt"
)

func Run() {
	systray.Run(onReady, onExit)
}

func getSampleRate() float64 {
	host, err := portaudio.DefaultHostApi()
	if err != nil {
		log.Fatalf("Error getting default host API: %v\n", err)
	}
	return host.DefaultInputDevice.DefaultSampleRate
}

func recordStream(stop <-chan struct{}) <-chan []float32 {
	out := make(chan []float32)

	go func() {
		defer close(out)

		host, err := portaudio.DefaultHostApi()
		if err != nil {
			log.Fatalf("Error getting default host API: %v\n", err)
		}

		params := portaudio.LowLatencyParameters(host.DefaultInputDevice, host.DefaultOutputDevice)
		params.Input.Channels = 1

		stream, err := portaudio.OpenStream(
			params,
			func(in []float32, _ []float32) {
				out <- in
			},
		)
		if err != nil {
			log.Fatalf("Error opening stream: %v\n", err)
		}

		defer func() {
			stream.Stop()
			stream.Close()
		}()

		stream.Start()

		for {
			select {
			case <-stop:
				return
			}
		}
	}()

	return out
}

func record(startRecording <-chan struct{}, stopRecording <-chan struct{}, stopListening <-chan struct{}) <-chan []float32 {
	out := make(chan []float32)

	go func() {
		defer close(out)

		if err := portaudio.Initialize(); err != nil {
			log.Fatalf("Error initializing PortAudio: %v\n", err)
		}
		defer portaudio.Terminate()

		stopStream := make(chan struct{})
		var joinedSamples []float32

		var recorded <-chan []float32
		defer func() {
			if recorded != nil {
				stopStream <- struct{}{}
			}
		}()

		recording := false
		recorded = recordStream(stopStream)

		for {
			select {
			case <-startRecording:
				if !recording {
					recording = true
					joinedSamples = make([]float32, 0)
				}
			case <-stopRecording:
				if recording {
					recording = false
					out <- joinedSamples
					joinedSamples = nil
				}
			case <-stopListening:
				return
			case samples := <-recorded:
				if joinedSamples != nil {
					joinedSamples = append(joinedSamples, samples...)
				}
			}
		}
	}()

	return out
}

func samplesToBase64Wav(samples []float32) (string, error) {
	ws := writerseeker.WriterSeeker{}
	encoder := wav.NewEncoder(&ws, int(getSampleRate()), 32, 1, 1)

	defer encoder.Close()

	// Convert float32 samples to an integer buffer (32-bit)
	// The go-audio/wav package expects int samples when writing (unless you use a FloatBuffer),
	// so we convert float [-1.0, 1.0] to int32.
	intBuf := make([]int, len(samples))
	for i, f := range samples {
		if f > 1.0 {
			f = 1.0
		} else if f < -1.0 {
			f = -1.0
		}
		intBuf[i] = int(f * float32((1<<31)-1))
	}

	// Construct an audio.IntBuffer that describes our audio data
	audioBuf := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: 1,
			SampleRate:  int(getSampleRate()),
		},
		SourceBitDepth: 32,
		Data:           intBuf,
	}

	// Write the samples to the WAV encoder
	if err := encoder.Write(audioBuf); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}

	// Retrieve the raw WAV data
	wavData, err := io.ReadAll(ws.Reader())
	if err != nil {
		return "", err
	}

	// Encode the WAV data as base64
	base64Wav := base64.StdEncoding.EncodeToString(wavData)
	return base64Wav, nil
}

type Audio struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type Content struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`        // Omit if empty
	InputAudio *InputAudio `json:"input_audio,omitempty"` // Omit if empty
}

type Message struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

type Payload struct {
	Model      string    `json:"model"`
	Modalities []string  `json:"modalities"`
	Audio      Audio     `json:"audio"`
	Messages   []Message `json:"messages"`
}

type Choice struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

type Choices []Choice

type Response struct {
	Choices Choices `json:"choices"`
}

func onReady() {
	systray.SetTitle("GPT")
	systray.SetTooltip("GPT")

	quitItem := systray.AddMenuItem("Quit", "Quit the whole app")
	go func() {
		<-quitItem.ClickedCh
		systray.Quit()
	}()

	events := hook.Start()

	go func() {
		defer hook.End()

		ctrlDown := false

		startRecording := make(chan struct{})
		stopRecording := make(chan struct{})
		stopListening := make(chan struct{})

		recording := record(startRecording, stopRecording, stopListening)

		defer func() {
			stopListening <- struct{}{}
		}()

		for {
			select {
			case event := <-events:
				// ctrl
				if event.Rawcode == 59 {
					if event.Kind == hook.KeyDown || event.Kind == hook.KeyHold {
						ctrlDown = true
					}
					if event.Kind == hook.KeyUp {
						ctrlDown = false
						stopRecording <- struct{}{}
					}
				}
				// escape
				if event.Rawcode == 53 {
					if ctrlDown {
						if event.Kind == hook.KeyDown || event.Kind == hook.KeyHold {
							startRecording <- struct{}{}
						}
					}
				}
			case samples := <-recording:
				base64Wav, err := samplesToBase64Wav(samples)
				if err != nil {
					log.Fatalf("Error converting samples to WAV: %v\n", err)
				}
				payload := Payload{
					Model:      "gpt-4o-audio-preview",
					Modalities: []string{"text"},
					Audio: Audio{
						Voice:  "alloy",
						Format: "wav",
					},
					Messages: []Message{
						{
							Role: "user",
							Content: []Content{
								{
									Type: "input_audio",
									InputAudio: &InputAudio{
										Data:   base64Wav,
										Format: "wav",
									},
								},
							},
						},
					},
				}

				jsonPayload, err := json.Marshal(payload)
				if err != nil {
					log.Fatalf("Error marshalling payload: %v\n", err)
					continue
				}

				apiUrl := "https://api.openai.com/v1/chat/completions"
				headers := map[string]string{
					"Content-Type": "application/json",
					"Authorization": "Bearer " + os.Getenv("OPENAI_API_KEY"),
				}
				req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonPayload))
				if err != nil {
					log.Fatalf("Error creating request: %v\n", err)
					continue
				}
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					log.Fatalf("Error making request: %v\n", err)
					continue
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Fatalf("Error reading response body: %v\n", err)
					continue
				}
				var response Response
				err = json.Unmarshal(body, &response)
				if err != nil {
					log.Fatalf("Error unmarshalling response: %v\n", err)
					continue
				}
				text := response.Choices[0].Message.Content
				fmt.Printf("%s\n", text)
			}
		}
	}()
}

func onExit() {
}
