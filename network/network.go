// Copyright 2018 Andrew Bates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package network

import (
	"io"
	"time"

	"github.com/abates/insteon"
)

// PacketRequest is used to request that a packetized (marshaled) insteon
// message be sent to the network. Once the upstream device (PLM usually)
// has attempted to send the packet, the Err field will be assigned and
// DoneCh will be written to and closed
type PacketRequest struct {
	Payload []byte
	Err     error
	DoneCh  chan<- *PacketRequest
}

// MessageRequest is used to request a message be sent to a specific device.
// Once the connection has sent the message and either received an ack or
// encountered an error, the Ack and Err fields will be filled and DoneCh
// will be written to and closed
type MessageRequest struct {
	Message *insteon.Message
	timeout time.Time
	Ack     *insteon.Message
	Err     error
	DoneCh  chan<- *MessageRequest
}

// Network is the main means to communicate with
// devices on the Insteon network
type Network struct {
	timeout     time.Duration
	DB          ProductDatabase
	connections []insteon.Connection

	bridge       insteon.Bridge
	connectCh    chan insteon.Connection
	disconnectCh chan insteon.Connection
	closeCh      chan chan error
}

// New creates a new Insteon network instance for the send and receive channels.  The timeout
// indicates how long the network (and subsuquent devices) should wait when expecting incoming
// messages/responses
func New(bridge insteon.Bridge, timeout time.Duration) *Network {
	network := &Network{
		timeout: timeout,
		DB:      NewProductDB(),
		bridge:  bridge,

		connectCh:    make(chan insteon.Connection),
		disconnectCh: make(chan insteon.Connection),
		closeCh:      make(chan chan error),
	}

	go network.process()
	return network
}

func (network *Network) process() {
	defer network.close()
	for {
		select {
		case buf := <-network.bridge.Receive():
			network.receive(buf)
		case connection := <-network.connectCh:
			network.connections = append(network.connections, connection)
		case connection := <-network.disconnectCh:
			network.disconnect(connection)
		case ch := <-network.closeCh:
			ch <- network.close()
			return
		}
	}
}

func (network *Network) receive(buf []byte) {
	msg := &insteon.Message{}
	err := msg.UnmarshalBinary(buf)
	if err == nil {
		insteon.Log.Tracef("Received Insteon Message %v", msg)
		if msg.Broadcast() {
			// Set Button Pressed Controller/Responder
			if msg.Command[1] == 0x01 || msg.Command[1] == 0x02 {
				network.DB.UpdateFirmwareVersion(msg.Src, insteon.FirmwareVersion(msg.Dst[2]))
				network.DB.UpdateDevCat(msg.Src, insteon.DevCat{msg.Dst[0], msg.Dst[1]})
			}
		} else if msg.Ack() && msg.Command[1] == 0x0d {
			// Engine Version Request ACK
			network.DB.UpdateEngineVersion(msg.Src, insteon.EngineVersion(msg.Command[2]))
		}

		for _, connection := range network.connections {
			// TODO: FIX THIS
			connection.Push(msg)
		}
	} else {
		insteon.Log.Infof("Failed to unmarshal buffer: %v", err)
	}
}

func (network *Network) disconnect(connection insteon.Connection) {
	for i, conn := range network.connections {
		if conn == connection {
			if closer, ok := conn.(io.Closer); ok {
				closer.Close()
			}
			network.connections = append(network.connections[0:i], network.connections[i+1:]...)
			break
		}
	}
}

/*func (network *Network) sendMessage(msg *insteon.Message) error {
	buf, err := msg.MarshalBinary()

	if err == nil {
		insteon.Log.Tracef("Sending %v to network", msg)
		if info, found := network.DB.Find(msg.Dst); found {
			if msg.Flags.Extended() && info.EngineVersion == insteon.VerI2Cs {
				buf[len(buf)-1] = checksum(buf[7:22])
			}
		}
		insteon.Log.Tracef("Sending %v to network", msg)
		err = network.bridge.Send(buf)
	}
	return err
}*/

// EngineVersion will query the dst device to determine its Insteon engine
// version
func (network *Network) EngineVersion(dst insteon.Address) (engineVersion insteon.EngineVersion, err error) {
	/*conn := network.connect(dst, 1, CmdGetEngineVersion)
	defer func() { close(conn.sendCh) }()

	doneCh := make(chan *MessageRequest, 1)
	request := &MessageRequest{Message: &Message{Command: CmdGetEngineVersion, Flags: StandardDirectMessage}, DoneCh: doneCh}
	conn.sendCh <- request
	<-doneCh

	if request.Err == nil {
		engineVersion = EngineVersion(request.Ack.Command[2])
	}*/
	return engineVersion, nil
}

// IDRequest will send an ID Request message to the destination device and wait for
// either a "Set-button Pressed Controller" or "Set-button Pressed Responder" broadcast
// message. This message includes the device category and firmaware information which
// is then returned in the DeviceInfo object.  It should be noted that the returned
// DeviceInfo object will not have the engine version field populated as this information
// is not included in the broadcast response.
func (network *Network) IDRequest(dst insteon.Address) (info insteon.DeviceInfo, err error) {
	info = insteon.DeviceInfo{
		Address: dst,
	}
	conn := network.connect(dst, 1, insteon.CmdSetButtonPressedResponder, insteon.CmdSetButtonPressedController)

	_, err = conn.Send(&insteon.Message{Command: insteon.CmdIDRequest, Flags: insteon.StandardDirectMessage})
	timeout := time.Now().Add(network.timeout)
	for err == nil {
		var msg *insteon.Message
		msg, err = conn.Receive()
		if err == nil {
			if msg.Broadcast() {
				info, _ = network.DB.Find(dst)
				return
			} else if timeout.Before(time.Now()) {
				err = insteon.ErrReadTimeout
			}
		}
	}

	return
}

func (network *Network) connect(dst insteon.Address, version insteon.EngineVersion, match ...insteon.Command) insteon.Connection {
	connection := insteon.NewConnection(network.bridge, dst, version, network.timeout, match...)
	network.connectCh <- connection
	return connection
}

// Dial will return a basic device object that can appropriately communicate
// with the physical device out on the insteon network. Dial will determine
// the engine version (1, 2, or 2CS) that the device is running and return
// either an I1Device, I2Device or I2CSDevice. For a fully initialized
// device (dimmer, switch, thermostat, etc) use Connect
func (network *Network) Dial(dst insteon.Address) (device insteon.Device, err error) {
	var info insteon.DeviceInfo
	var found bool
	if info, found = network.DB.Find(dst); !found {
		info.EngineVersion, err = network.EngineVersion(dst)
		// ErrNotLinked here is only returned by i2cs devices
		if err == insteon.ErrNotLinked {
			network.DB.UpdateEngineVersion(dst, insteon.VerI2Cs)
			info.EngineVersion = insteon.VerI2Cs
		}
	}

	if err == nil || err == insteon.ErrNotLinked {
		connection := network.connect(dst, info.EngineVersion)
		switch info.EngineVersion {
		case insteon.VerI1:
			device = insteon.NewI1Device(dst, connection, network.timeout)
		case insteon.VerI2:
			device = insteon.NewI2Device(dst, connection, network.timeout)
		case insteon.VerI2Cs:
			device = insteon.NewI2CsDevice(dst, connection, network.timeout)
		default:
			err = insteon.ErrVersion
		}
	}
	return device, err
}

// Connect will Dial the destination device and then determine the device category
// in order to return a category specific device (dimmer, switch, etc). If, for
// some reason, the devcat cannot be determined, then the device returned
// by Dial is returned
func (network *Network) Connect(dst insteon.Address) (device insteon.Device, err error) {
	var info insteon.DeviceInfo
	var found bool
	if info, found = network.DB.Find(dst); !found {
		info.EngineVersion, err = network.EngineVersion(dst)
		if err == nil {
			info, err = network.IDRequest(dst)
		}
	}

	if err == nil {
		if constructor, found := insteon.Devices.Find(info.DevCat.Category()); found {
			bridge := network.connect(dst, info.EngineVersion)
			device, err = constructor(info, dst, bridge, network.timeout)
		} else {
			device, err = network.Dial(dst)
		}
	}
	return
}

func (network *Network) close() error {
	network.connections = nil
	return nil
}

// Close will cleanup/close open connections and disconnect gracefully
func (network *Network) Close() error {
	ch := make(chan error)
	network.closeCh <- ch
	close(network.closeCh)
	err := <-ch
	if closer, ok := network.bridge.(io.Closer); ok {
		err1 := closer.Close()
		if err == nil {
			err = err1
		}
	}
	return err
}