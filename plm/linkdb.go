package plm

import (
	"fmt"
	"time"

	"github.com/abates/insteon"
)

type recordRequestCommand byte

const (
	LinkCmdFindFirst    recordRequestCommand = 0x00
	LinkCmdFindNext     recordRequestCommand = 0x01
	LinkCmdModFirst     recordRequestCommand = 0x20
	LinkCmdModFirstCtrl recordRequestCommand = 0x40
	LinkCmdModFirstResp recordRequestCommand = 0x41
	LinkCmdDeleteFirst  recordRequestCommand = 0x80
)

type manageRecordRequest struct {
	command recordRequestCommand
	link    *insteon.LinkRecord
}

func (mrr *manageRecordRequest) String() string {
	return fmt.Sprintf("%02x %s", mrr.command, mrr.link)
}

func (mrr *manageRecordRequest) MarshalBinary() ([]byte, error) {
	payload, err := mrr.link.MarshalBinary()
	buf := make([]byte, len(payload)+1)
	buf[0] = byte(mrr.command)
	copy(buf[1:], payload)
	return buf, err
}

func (mrr *manageRecordRequest) UnmarshalBinary(buf []byte) error {
	mrr.command = recordRequestCommand(buf[0])
	mrr.link = &insteon.LinkRecord{}
	return mrr.link.UnmarshalBinary(buf[1:])
}

type LinkDB struct {
	plm *PLM
}

func NewLinkDB(plm *PLM) *LinkDB {
	db := &LinkDB{plm}

	return db
}

func (db *LinkDB) Links() ([]*insteon.LinkRecord, error) {
	links := make([]*insteon.LinkRecord, 0)
	rrCh := db.plm.Subscribe([]byte{0x57})
	defer db.plm.Unsubscribe(rrCh)

	insteon.Log.Debugf("Retrieving PLM link database")
	resp, err := db.plm.Send(&Packet{Command: CmdGetFirstAllLink})
	if resp.NAK() {
		err = nil
	} else if err == nil {
	loop:
		for {
			select {
			case packet := <-rrCh:
				link := &insteon.LinkRecord{}
				err := link.UnmarshalBinary(packet.payload)
				if err == nil {
					insteon.Log.Debugf("Received PLM record response %v", link)
					links = append(links, link)
					var resp *Packet
					resp, err = db.plm.Send(&Packet{Command: CmdGetNextAllLink})
					if resp.NAK() || err != nil {
						break loop
					}
				} else {
					insteon.Log.Infof("Failed to unmarshal link record: %v", err)
					break loop
				}
			case <-time.After(insteon.Timeout):
				err = insteon.ErrReadTimeout
				break loop
			}
		}
	}
	return links, err
}

func (db *LinkDB) RemoveLinks(oldLinks ...*insteon.LinkRecord) (err error) {
	deletedLinks := make([]*insteon.LinkRecord, 0)
	for _, oldLink := range oldLinks {
		numDelete := 0
		var links []*insteon.LinkRecord
		links, err = db.Links()
		if err == nil {
			for _, link := range links {
				if link.Group == oldLink.Group && link.Address == oldLink.Address {
					numDelete++
					if !oldLink.Equal(link) {
						deletedLinks = append(deletedLinks, link)
					}
				}
			}

			for i := 0; i < numDelete; i++ {
				rr := &manageRecordRequest{command: LinkCmdDeleteFirst, link: oldLink}
				payload, _ := rr.MarshalBinary()
				_, err = db.plm.Send(&Packet{Command: CmdManageAllLinkRecord, payload: payload})
				if err != nil {
					insteon.Log.Infof("Failed to remove link: %v", err)
					break
				}
			}
		} else {
			insteon.Log.Infof("Failed to retrieve links: %v", err)
			break
		}
	}

	if err == nil {
		// add back links that we didn't want deleted
		for _, link := range deletedLinks {
			db.AddLink(link)
		}
	}
	return err
}

func (db *LinkDB) AddLink(newLink *insteon.LinkRecord) error {
	var command recordRequestCommand
	if newLink.Flags.Controller() {
		command = LinkCmdModFirstCtrl
	} else {
		command = LinkCmdModFirstResp
	}
	rr := &manageRecordRequest{command: command, link: newLink}
	payload, _ := rr.MarshalBinary()
	resp, err := db.plm.Send(&Packet{Command: CmdManageAllLinkRecord, payload: payload})

	if resp.NAK() {
		err = fmt.Errorf("Failed to add link back to ALDB")
	}

	return err
}

func (db *LinkDB) Cleanup() (err error) {
	removeable := make([]*insteon.LinkRecord, 0)
	links, err := db.Links()
	if err == nil {
		for i, l1 := range links {
			for _, l2 := range links[i+1:] {
				if l1.Equal(l2) {
					removeable = append(removeable, l2)
				}
			}
		}

		err = db.RemoveLinks(removeable...)
	}
	return err
}
