package core

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/steps0x29a/alohomora/gen"
	"github.com/steps0x29a/alohomora/msg"
	"github.com/steps0x29a/islazy/bigint"
	"github.com/steps0x29a/islazy/bytes"
	"github.com/steps0x29a/islazy/fio"
	"github.com/steps0x29a/islazy/term"

	"github.com/steps0x29a/alohomora/opts"

	uuid "github.com/satori/go.uuid"
)

// A Client is basically a socket connection with some
// additional info.
type Client struct {
	Socket     net.Conn
	ID         uuid.UUID
	Terminated chan bool
	Errors     chan error
	validated  chan bool
}

// ShortID returns the first eight characters of a client's ID.
// Inspired by git's short commit hashes.
func (client *Client) ShortID() string {
	return client.ID.String()[:8]
}

// FullID returns a client's full ID consisting of its short ID (the
// first eight characters of its full ID) and its socket remote address.
func (client *Client) FullID() string {
	return fmt.Sprintf("%s | %s", client.ShortID(), client.Socket.RemoteAddr().String())
}

// String returns the same as client.FullID(), basically its short ID and socket
// remote address.
func (client *Client) String() string {
	return client.FullID()
}

func newClient(socket net.Conn) *Client {
	return &Client{
		Socket:     socket,
		ID:         uuid.Nil,
		Terminated: make(chan bool),
		Errors:     make(chan error),
		validated:  make(chan bool),
	}
}

func generatePasswords(params *GenerationParams) (string, error) {
	path, err := fio.TempFilePath()
	if err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	defer fmt.Println()

	term.Info("Generating %d passwords...", params.Amount)

	var i int64 = 0
	for i = 0; i < params.Amount; i++ {
		pw, err := gen.GeneratePassword(params.Charset, params.Length, bigint.Add(params.Offset, big.NewInt(i)))
		if err != nil {
			return "", err
		}

		_, err = f.WriteString(fmt.Sprintf("%s\n", pw))
		if err != nil {
			return "", err
		}
	}

	//term.Info("Generated %d passwords\n", params.Amount)
	fmt.Printf(term.BrightGreen("OK"))
	return path, nil
}

func writeTmpBinFile(data []byte) (string, error) {
	path, err := fio.TempFilePath()
	if err != nil {
		return "", err
	}

	hs, err := os.Create(path)
	if err != nil {
		return "", err
	}

	defer hs.Close()

	_, err = hs.Write(data)
	if err != nil {
		return "", err
	}

	return path, nil

}

func (client *Client) work(job *CrackJob) ([]byte, error) {

	// Decode crackjob' payload
	if job.Type == WPA2 {
		term.Info("Working on %s (%s)...\n", term.BrightBlue(job.ID.String()[:8]), term.Cyan("WPA2"))

		// Generate passwords
		pwFilepath, err := generatePasswords(job.Gen)
		if err != nil {
			return nil, err
		}

		defer os.Remove(pwFilepath)

		// WPA2 payload
		payload, err := job.decodeWPA2()
		if err != nil {
			return nil, err
		}

		handshakeFilepath, err := writeTmpBinFile(payload.Data)
		if err != nil {
			return nil, err
		}

		defer os.Remove(handshakeFilepath)

		term.Info("Running aircrack-ng...")
		output, err := Aircrack(payload.BSSID, payload.ESSID, pwFilepath, handshakeFilepath)
		if err != nil {
			fmt.Printf("%s\n", term.BrightRed("ERROR"))
			return nil, err
		}
		fmt.Printf("%s\n", term.BrightGreen("OK"))

		password := KeyFromOutput(output)
		found := password != ""
		if found {
			term.Success("%s\n", term.LabelGreen("Cracked the password!"))
		} else {
			term.Problem("%s\n", term.BrightRed("Too bad, password not cracked"))
		}
		fmt.Println()
		result := &CrackJobResult{Payload: password, JobID: job.ID, Success: found}

		return result.encode()
	}

	term.Warn("Only WPA2 jobs are implemented as of now\n")
	return nil, errors.New("Not a WPA2 payload")
}

func (client *Client) handle(message *msg.Message) {
	switch message.Type {
	case msg.Ack:
		{
			client.validated <- true
			break
		}
	case msg.Leave:
		{
			term.Info("Server asked me to leave, closing connection...\n")
			client.Terminated <- true
			break
		}

	case msg.Task:
		{

			// Decode payload
			job, err := decodeJob(message.Payload)
			if err != nil {
				/// TODO: Send error message to server
				errMsg := msg.NewMessage(msg.ClientError, []byte(err.Error()))
				client.send(errMsg)
				client.Errors <- err
				return
			}

			term.Info("Received a new task: %s\n", term.BrightBlue(job.ID.String()[:8]))

			result, err := client.work(job)
			if err != nil {
				term.Error("Failed to attempt cracking: %s\n", err)
				result := CrackJobResult{JobID: job.ID, Payload: "", Success: false}
				encoded, err := result.encode()
				if err != nil {
					term.Error("Unable to encode crackjobresult: %s\n", err)
					client.Terminated <- true
					break
				}
				term.Info("Sending fail message\n")
				client.send(msg.NewMessage(msg.ClientError, encoded))
				break
			}
			answer := msg.NewMessage(msg.Finished, result)
			client.send(answer)
			break
		}
	}
}

func (client *Client) receive() {
	var buffer = make([]byte, bufferSize)

	for {
		var message = make([]byte, 0)
		var size = 0

		for {
			read, err := client.Socket.Read(buffer)
			if read == 0 || err != nil && err != io.EOF {
				// Connection lost
				client.Terminated <- true
				return
			}

			message = append(message, buffer[:read]...)
			size += read

			if bytes.EndsWith(message, AlohomoraSuffix) {
				decoded, err := msg.Decode(message[:size-len(AlohomoraSuffix)])
				if err != nil {
					term.Error("Unable to decode server message: %s\n", term.BrightRed(fmt.Sprintf("%s", err)))
				} else {
					client.handle(decoded)
				}

				break
			}

		}

	}
}

func (client *Client) send(message *msg.Message) {
	data, err := message.Encode()
	if err != nil {
		term.Error("Unable to encode message: %s\n", err)
		client.Errors <- err
		return
	}

	_, err = client.Socket.Write(data)
	// TODO: Handle incomplete writes
	if err != nil {
		term.Error("Unable to send message: %s\n", err)
		client.Errors <- err
		return
	}

	_, err = client.Socket.Write(AlohomoraSuffix)
	if err != nil {
		term.Error("Unable to send suffix: %s\n", err)
		client.Errors <- err
		return
	}
}

// Connect tries to establish a connection to a server.
// The server's IP and port are provided via an Options instance.
func Connect(opts *opts.Options) (*Client, error) {
	dialer := net.Dialer{
		Timeout: time.Second * 30,
	}

	term.Info("Connecting to %s:%d...\n", opts.Host, opts.Port)
	socket, err := dialer.Dial("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port))
	if err != nil {
		return nil, err
	}

	client := newClient(socket)
	go client.receive()
	go client.send(msg.NewMessage(msg.Hello, nil))

	<-client.validated
	term.Success(term.BrightGreen("Connection established!\n"))

	// Tell server we are ready for action
	go client.send(msg.NewMessage(msg.Idle, nil))

	return client, nil
}