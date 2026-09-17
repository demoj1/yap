package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"
)

// Chat: text between the people in the room, plus the room's own events
// spoken by "system". Messages ride the same encrypted pair channels as
// audio (relayed when needed); UDP may lose one, so each goes out three
// times and the receiver drops repeats.

const chatKeep = 200

type chatMsg struct {
	At   time.Time
	From string // "" is system
	Text string
}

type chat struct {
	mu   sync.Mutex
	msgs []chatMsg
}

func (c *chat) add(from, text string) {
	c.mu.Lock()
	c.msgs = append(c.msgs, chatMsg{time.Now(), from, text})
	if len(c.msgs) > chatKeep {
		c.msgs = c.msgs[len(c.msgs)-chatKeep:]
	}
	c.mu.Unlock()
}

func (c *chat) tail(n int) []chatMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chatMsg(nil), c.msgs[max(0, len(c.msgs)-n):]...)
}

// system logs an event and says it in the chat.
func (n *node) system(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	log.Println(s)
	n.chat.add("", s)
}

// say sends text to everyone in the room.
func (n *node) say(text string) {
	if text == "" {
		return
	}
	n.chat.add(n.name, text)
	id := n.chatSeq.Add(1)
	payload := make([]byte, 5, 5+len(text))
	payload[0] = typChat
	binary.BigEndian.PutUint32(payload[1:], id)
	payload = append(payload, text...)
	go func() {
		for i := 0; i < 3; i++ {
			for _, p := range n.people() {
				if p.connected() {
					n.sendTo(p, payload)
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// heard takes a chat payload from p: the id weeds out the repeats.
func (n *node) heard(p *peer, plain []byte) {
	if len(plain) < 5 {
		return
	}
	id := binary.BigEndian.Uint32(plain[1:])
	for _, s := range p.chatSeen {
		if s == id {
			return
		}
	}
	p.chatSeen[p.chatIdx%len(p.chatSeen)] = id
	p.chatIdx++
	n.chat.add(p.name, string(plain[5:]))
}
