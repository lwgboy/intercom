package intercom

import (
	"context"
	"fmt"
	"image"
	"io"
	"math"
	"time"

	"github.com/3xcellent/intercom/proto"

	"github.com/gordonklaus/portaudio"
	"gocv.io/x/gocv"
	"google.golang.org/grpc"
)

const (
	screenWidth  = 1280 / 2
	screenHeight = 720 / 2

	outPreviewWidth  = screenWidth / 4
	outPreviewHeight = screenHeight / 4
	outPreviewX      = screenHeight - outPreviewHeight - (outPreviewHeight / 4)
	outPreviewY      = screenWidth - outPreviewWidth - (outPreviewWidth / 4)

	inBroadcastWidth  = screenWidth / 2
	inBroadcastHeight = screenHeight / 2
	inBroadcastX      = screenHeight/2 - inBroadcastHeight/2 - inBroadcastHeight/4
	inBroadcastY      = screenWidth/2 - inBroadcastWidth/2 - inBroadcastWidth/4

	sampleRate    = 44100
	sampleSeconds = .1

	matType = gocv.MatTypeCV8UC3
)

type intercomClient struct {
	window            *gocv.Window
	webcam            *gocv.VideoCapture
	audioInputStream  *portaudio.Stream
	audioOutputStream *portaudio.Stream
	deviceID          string

	context context.Context

	intercomServer proto.Intercom_ConnectClient

	bgImg           gocv.Mat
	displayImg      gocv.Mat
	videoPreviewImg gocv.Mat
	inBroadcastImg  gocv.Mat

	audioOutputCache [][]int32

	lastInBroadcastTime time.Time

	isReceivingBroadcast bool
	hasWebcamOn          bool
	hasMicOn             bool
	isPlayingAudio       bool
	wantToBroadcast      bool
	wantToQuit           bool
}

func CreateIntercomClient(ctx context.Context, vidoeCaptureDeviceId, filename string) intercomClient {
	client := intercomClient{
		window:          gocv.NewWindow("Capture Window"),
		deviceID:        vidoeCaptureDeviceId,
		videoPreviewImg: gocv.NewMatWithSize(outPreviewHeight, outPreviewWidth, gocv.MatTypeCV8UC3),
		inBroadcastImg:  gocv.NewMatWithSize(inBroadcastHeight, inBroadcastWidth, gocv.MatTypeCV8UC3),
		context:         ctx,
	}

	client.loadBackgroundImg(filename)

	return client
}

func (c *intercomClient) loadBackgroundImg(path string) {
	c.bgImg = gocv.NewMatWithSize(screenHeight, screenWidth, matType)
	defaultImg := gocv.IMRead(path, gocv.IMReadColor)
	defer defaultImg.Close()

	if defaultImg.Empty() {
		fmt.Printf("Error reading image from: %v\n", path)
		return
	} else {
		fmt.Printf("Opening image from: %v | %#v\n", path, defaultImg.Size())
	}
	gocv.Resize(defaultImg, &c.bgImg, image.Point{X: screenWidth, Y: screenHeight}, 0, 0, gocv.InterpolationDefault)
	c.ResetDisplayImg()
}

func (c *intercomClient) shutdown() {
	if c.hasWebcamOn {
		c.hasWebcamOn = false
		c.webcam.Close()
	}

	c.bgImg.Close()
	c.displayImg.Close()
	c.videoPreviewImg.Close()
	c.inBroadcastImg.Close()

	c.window.Close()
}

func (c *intercomClient) connectToServer() {
	// dail server
	conn, err := grpc.Dial(":6000", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	// create streams
	client := proto.NewIntercomClient(conn)
	c.intercomServer, err = client.Connect(c.context)
	if err != nil {
		panic(err)
	}
}

func (c *intercomClient) ResetDisplayImg() {
	c.displayImg = c.bgImg.Clone()
}

func (c *intercomClient) handleGrpcStreamRec() {
	for {
		resp, err := c.intercomServer.Recv()
		if err == io.EOF {
			c.ResetDisplayImg()
			continue
		}
		if err != nil {
			panic(err)
		}

		c.lastInBroadcastTime = time.Now()

		respImage := resp.GetImage()
		if respImage != nil {
			c.processBroadcastImage(*respImage)
			continue
		}

		respAudio := resp.GetAudio()
		if respAudio != nil {
			c.audioOutputCache = append(c.audioOutputCache, respAudio.Samples)
			if !c.isPlayingAudio {
				c.isPlayingAudio = true
				go c.playAudio()
			}
		}
	}
}

func (c *intercomClient) processBroadcastImage(img proto.Image) {
	serverImg, err := gocv.NewMatFromBytes(int(img.Height),
		int(img.Width),
		gocv.MatType(img.Type),
		img.Bytes)
	if err != nil {
		fmt.Printf("cannot create NewMatFromBytes %v\n", err)
		c.ResetDisplayImg()
		return
	}
	defer serverImg.Close()

	if serverImg.Empty() {
		c.isReceivingBroadcast = false
		c.ResetDisplayImg()
		fmt.Println("incoming broadcast ended")
		return
	}

	if !c.isReceivingBroadcast {
		c.isReceivingBroadcast = true
		fmt.Println("receiving incoming broadcast")
	}

	screenCapRatio := float64(float64(serverImg.Size()[1]) / float64(serverImg.Size()[0]))
	scaledHeight := int(math.Floor(inBroadcastWidth / screenCapRatio))

	gocv.Resize(serverImg, &c.inBroadcastImg, image.Point{X: inBroadcastWidth, Y: scaledHeight}, 0, 0, gocv.InterpolationDefault)
}

func (c *intercomClient) playAudio() {
	out := make([]int32, sampleRate*sampleSeconds)
	var err error

	c.audioOutputStream, err = portaudio.OpenDefaultStream(0, 1, sampleRate, len(out), &out)
	if err != nil {
		panic("audio out err: " + err.Error())
	}
	defer c.audioOutputStream.Close()

	c.audioOutputStream.Start()
	defer c.audioOutputStream.Stop()

	// audio playback loop
	for {
		cacheLength := len(c.audioOutputCache)
		if cacheLength == 0 {
			c.isPlayingAudio = false
			break
		}

		c.isPlayingAudio = true

		out = c.audioOutputCache[0]
		c.audioOutputCache = c.audioOutputCache[1:]
		err = c.audioOutputStream.Write()
		if err != nil {
			panic("playback err: " + err.Error())
		}
	}
}

func (c *intercomClient) startAudioBroadcast() {
	c.hasMicOn = true
	in := make([]int32, 44100*.1)
	fmt.Println("OpenDefaultStream...")
	audioInStream, err := portaudio.OpenDefaultStream(1, 0, 44100, len(in), &in)
	if err != nil {
		panic(err)
	}
	err = audioInStream.Start()
	if err != nil {
		panic(err)
	}

	// audio broadcast loop
	for {
		select {
		case <-c.context.Done():
			break
		default:
		}

		if !c.wantToBroadcast {
			break
		}

		err = audioInStream.Read()
		if err != nil {
			panic(err)
		}

		go func(sendSamples []int32) {
			req := proto.Broadcast{
				BroadcastType: &proto.Broadcast_Audio{
					Audio: &proto.Audio{
						Samples: sendSamples,
					},
				},
			}

			if err := c.intercomServer.Send(&req); err != nil {
				fmt.Printf("Send error: %v", err)
				return
			}
			if err != nil {
				panic(err)
			}
		}(in)
	}
	err = audioInStream.Stop()
	if err != nil {
		panic(err)
	}
	c.hasMicOn = false
}

func (c *intercomClient) sendVideoCapture() {
	if !c.hasWebcamOn {
		var err error
		c.webcam, err = gocv.OpenVideoCapture(c.deviceID)
		if err != nil {
			fmt.Printf("Error opening video capture device: %v\n", c.deviceID)
			return
		}
		c.hasWebcamOn = true
		fmt.Println("outgoing broadcast starting")
	}

	videoCaptureImg := gocv.NewMat()
	defer videoCaptureImg.Close()

	if ok := c.webcam.Read(&videoCaptureImg); !ok {
		fmt.Println("didn't read from cam")
	}

	if videoCaptureImg.Empty() {
		if c.hasWebcamOn {
			c.webcam.Close()
			c.hasWebcamOn = false
			fmt.Println("outgoing broadcast ended")
		}
		return
	}

	req := proto.Broadcast{
		BroadcastType: &proto.Broadcast_Image{
			Image: &proto.Image{
				Height: int32(videoCaptureImg.Size()[0]),
				Width:  int32(videoCaptureImg.Size()[1]),
				Type:   int32(videoCaptureImg.Type()),
				Bytes:  videoCaptureImg.ToBytes(),
			},
		},
	}

	if err := c.intercomServer.Send(&req); err != nil {
		fmt.Printf("Send error: %v", err)
		return
	}


	screenCapRatio := float64(float64(videoCaptureImg.Size()[1]) / float64(videoCaptureImg.Size()[0]))
	outPreviewScaledHeight := int(math.Floor(outPreviewWidth / screenCapRatio))

	gocv.Resize(videoCaptureImg, &c.videoPreviewImg, image.Point{X: outPreviewWidth, Y: outPreviewScaledHeight}, 0, 0, gocv.InterpolationDefault)
}

func (c *intercomClient) draw() {
	if c.hasIncomingBroadcast() {
		for x := 0; x < c.inBroadcastImg.Size()[0]; x++ {
			for y := 0; y < inBroadcastWidth; y++ {
				c.displayImg.SetIntAt3(x+inBroadcastX, y+inBroadcastY, 0, c.inBroadcastImg.GetIntAt3(x, y, 0))
			}
		}
	}

	if c.hasWebcamOn {
		for x := 0; x < c.videoPreviewImg.Size()[0]; x++ {
			for y := 0; y < outPreviewWidth; y++ {
				c.displayImg.SetIntAt3(x+outPreviewX, y+outPreviewY, 0, c.videoPreviewImg.GetIntAt3(x, outPreviewWidth-y, 0))
			}
		}
	}

	c.window.IMShow(c.displayImg)
}

func (c *intercomClient) hasIncomingBroadcast() bool {
	if !c.isReceivingBroadcast {
		return false
	}
	if time.Now().After(c.lastInBroadcastTime.Add(300 * time.Millisecond)) {
		c.isReceivingBroadcast = false
		c.ResetDisplayImg()
		return false
	}
	return true
}

func (c *intercomClient) Run() {
	c.connectToServer()

	portaudio.Initialize()
	defer portaudio.Terminate()

	go c.handleGrpcStreamRec()

	// main program loop
	for {
		select {
		case <-c.context.Done():
			c.wantToQuit = true
			return
		default:
		}

		switch c.window.WaitKey(1) {
		case 27:
			c.wantToQuit = true
		case 32:
			c.wantToBroadcast = !c.wantToBroadcast
		default:
		}

		if c.wantToQuit {
			c.shutdown()
			break
		}

		if c.wantToBroadcast {
			c.sendVideoCapture()
			if !c.hasMicOn {
				fmt.Println("go c.startAudioBroadcast()...")
				go c.startAudioBroadcast()
			}
		} else {
			if c.hasMicOn {
				c.hasMicOn = false
			}
			if c.hasWebcamOn {
				c.webcam.Close()
				c.hasWebcamOn = false
				c.ResetDisplayImg()
			}
		}
		c.draw()
	}
}
