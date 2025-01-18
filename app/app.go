package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"github.com/getlantern/systray"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/gordonklaus/portaudio"
	"github.com/orcaman/writerseeker"
	hook "github.com/robotn/gohook"
	"io"
	"log"
	"net/http"
	"os"
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

		params := portaudio.LowLatencyParameters(host.DefaultInputDevice, nil)
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

func playSamples(samples []float32, sampleRate float64) error {
	host, err := portaudio.DefaultHostApi()
	if err != nil {
		return err
	}

	params := portaudio.LowLatencyParameters(nil, host.DefaultOutputDevice)

	params.Output.Channels = 1
	params.SampleRate = sampleRate

	i := 0

	done := make(chan struct{})

	stream, err := portaudio.OpenStream(
		params,
		func(_ []float32, out []float32) {
			for sample := range out {
				if i >= len(samples) {
					done <- struct{}{}
					return
				}
				out[sample] = samples[i]
				i++
			}
		},
	)
	if err != nil {
		return err
	}

	defer func() {
		stream.Stop()
		stream.Close()
	}()

	stream.Start()

	<-done

	return nil
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

func base64WavToSamples(base64Wav string) ([]float32, uint32, error) {
	// Decode the base64 WAV data
	wavData, err := base64.StdEncoding.DecodeString(base64Wav)
	if err != nil {
		return nil, 0, err
	}

	// Create a reader for the WAV data
	r := bytes.NewReader(wavData)

	// Create a decoder for the WAV data
	decoder := wav.NewDecoder(r)

	// Read the WAV data
	audioBuf, err := decoder.FullPCMBuffer()
	if err != nil {
		return nil, 0, err
	}

	// Convert the audio data to float32 samples
	samples := make([]float32, len(audioBuf.Data))
	maxV := float32(0)
	for i, v := range audioBuf.Data {
		vFloat := float32(v)
		samples[i] = vFloat
		if vFloat > maxV {
			maxV = vFloat
		}
	}

	for i, v := range samples {
		samples[i] = v / maxV
	}

	sampleRate := decoder.SampleRate

	return samples, sampleRate, nil
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

type ResponseAudio struct {
	Data string `json:"data"`
}

type Choice struct {
	Message struct {
		Content string        `json:"content"`
		Audio   ResponseAudio `json:"audio"`
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
		altDown := false
		metaDown := false

		startRecording := make(chan struct{})
		stopRecording := make(chan struct{})
		stopListening := make(chan struct{})

		recordedSamples := record(startRecording, stopRecording, stopListening)
		recording := false

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
					}
				}
				// alt
				if event.Rawcode == 58 {
					if event.Kind == hook.KeyDown || event.Kind == hook.KeyHold {
						altDown = true
					}
					if event.Kind == hook.KeyUp {
						altDown = false
					}
				}
				// meta
				if event.Rawcode == 55 {
					if event.Kind == hook.KeyDown || event.Kind == hook.KeyHold {
						metaDown = true
					}
					if event.Kind == hook.KeyUp {
						metaDown = false
					}
				}

				if !recording && ctrlDown && altDown && metaDown {
					recording = true
					startRecording <- struct{}{}
					log.Println("Recording")
				}
				if recording && !ctrlDown && !altDown && !metaDown {
					stopRecording <- struct{}{}
					recording = false
					log.Println("Stopped recording")
				}
			case samples := <-recordedSamples:
				go func() {
					log.Printf("Recorded %d samples\n", len(samples))
					base64Wav, err := samplesToBase64Wav(samples)
					if err != nil {
						log.Fatalf("Error converting samples to WAV: %v\n", err)
					}
					payload := Payload{
						Model:      "gpt-4o-mini-audio-preview",
						Modalities: []string{"text", "audio"},
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
						return
					}

					apiUrl := "https://api.openai.com/v1/chat/completions"
					headers := map[string]string{
						"Content-Type":  "application/json",
						"Authorization": "Bearer " + os.Getenv("OPENAI_API_KEY"),
					}
					req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonPayload))
					if err != nil {
						log.Fatalf("Error creating request: %v\n", err)
						return
					}
					for k, v := range headers {
						req.Header.Set(k, v)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						log.Fatalf("Error making request: %v\n", err)
						return
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						log.Fatalf("Error reading response body: %v\n", err)
						return
					}
					var response Response
					err = json.Unmarshal(body, &response)
					if err != nil {
						log.Fatalf("Error unmarshalling response: %v\n", err)
						return
					}
					data := response.Choices[0].Message.Audio.Data
					samples, sampleRate, err := base64WavToSamples(data)
					if err != nil {
						log.Fatalf("Error converting base64 WAV to samples: %v\n", err)
						return
					}
					log.Printf("Playing %d samples at %d Hz\n", len(samples), sampleRate)
					err = playSamples(samples, float64(sampleRate))
					if err != nil {
						log.Fatalf("Error playing samples: %v\n", err)
					}
				}()
			}
		}
	}()
}

func onExit() {
}
