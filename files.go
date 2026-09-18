package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Files ride the same encrypted pair channels as audio, cut into chunks.
// UDP loses some, so the receiver asks for what is missing until it has
// everything, then says so. Every file lands in <config>/files and shows
// up in the chat (pictures inline on the web page).

const (
	fileChunk = 1024                   // bytes of payload per packet: relayed and sealed it stays under the MTU
	fileMax   = 32 << 20               // refuse anything bigger
	filePace  = 8 * time.Millisecond   // between chunks to one receiver: ~1 Mbit/s, voice still fits beside it
	fileQuiet = 300 * time.Millisecond // receiver silence before asking for the missing chunks
	fileGrace = 30 * time.Second       // either side gives up after this long without progress
)

// Kinds of typFile packets: [7][kind][file id u32]...
const (
	fileHead = iota // [count u16][size u32][name]  — sent first and again on request
	fileData        // [seq u16][bytes]
	fileMiss        // [seq u16]...  — what the receiver still lacks; empty means "send the head again"
	fileDone        // the receiver has everything
)

type fileKey struct {
	p  *peer
	id uint32
}

type inFile struct {
	name   string
	size   int
	chunks [][]byte // by seq; nil while missing
	got    int
	last   time.Time // when the last packet arrived
}

type files struct {
	mu   sync.Mutex
	dir  string
	seq  uint32
	in   map[fileKey]*inFile
	miss map[fileKey]chan []uint16 // the sender's ear for each receiver's requests
}

func newFiles() *files {
	dir := filepath.Join(configDir(), "files")
	must(os.MkdirAll(dir, 0o700))
	return &files{dir: dir, in: map[fileKey]*inFile{}, miss: map[fileKey]chan []uint16{}}
}

// store writes data under a name that cannot escape the directory and
// returns the file name the chat and the web page refer to.
func (f *files) store(id uint32, name string, data []byte) (string, error) {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < ' ' {
			return '_'
		}
		return r
	}, filepath.Base(name))
	name = fmt.Sprintf("%08x-%s", id, name)
	return name, os.WriteFile(filepath.Join(f.dir, name), data, 0o600)
}

func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// sendFile keeps a copy, says it in the chat and pushes it to everyone.
func (n *node) sendFile(name string, data []byte) error {
	if len(data) == 0 || len(data) > fileMax {
		return fmt.Errorf("file must be 1 B – %s", sizeText(fileMax))
	}
	f := n.files
	f.mu.Lock()
	f.seq++
	id := f.seq
	f.mu.Unlock()
	stored, err := f.store(id, name, data)
	if err != nil {
		return err
	}
	n.chat.addFile(n.name, stored, len(data))
	for _, p := range n.people() {
		if p.connected() {
			go n.pushFile(p, id, name, data)
		}
	}
	return nil
}

// pushFile sends one file to one person: everything once, then whatever
// they ask for again, until they say done or stop answering.
func (n *node) pushFile(p *peer, id uint32, name string, data []byte) {
	count := (len(data) + fileChunk - 1) / fileChunk
	head := make([]byte, 0, 12+len(name))
	head = append(head, typFile, fileHead)
	head = binary.BigEndian.AppendUint32(head, id)
	head = binary.BigEndian.AppendUint16(head, uint16(count))
	head = binary.BigEndian.AppendUint32(head, uint32(len(data)))
	head = append(head, name...)
	chunk := func(seq int) []byte {
		b := make([]byte, 0, 8+fileChunk)
		b = append(b, typFile, fileData)
		b = binary.BigEndian.AppendUint32(b, id)
		b = binary.BigEndian.AppendUint16(b, uint16(seq))
		return append(b, data[seq*fileChunk:min(len(data), (seq+1)*fileChunk)]...)
	}
	key := fileKey{p, id}
	ear := make(chan []uint16, 16)
	n.files.mu.Lock()
	n.files.miss[key] = ear
	n.files.mu.Unlock()
	defer func() {
		n.files.mu.Lock()
		delete(n.files.miss, key)
		n.files.mu.Unlock()
	}()
	n.sendTo(p, head)
	for seq := 0; seq < count; seq++ {
		n.sendTo(p, chunk(seq))
		time.Sleep(filePace)
	}
	deadline := time.Now().Add(fileGrace)
	for time.Now().Before(deadline) {
		select {
		case want, ok := <-ear:
			if !ok {
				return // done
			}
			deadline = time.Now().Add(fileGrace)
			n.sendTo(p, head)
			for _, seq := range want {
				if int(seq) < count {
					n.sendTo(p, chunk(int(seq)))
					time.Sleep(filePace)
				}
			}
		case <-time.After(2 * time.Second):
			n.sendTo(p, head) // nudge: a lost head leaves them unable to ask
		}
	}
	log.Printf("file %s: %s never confirmed", name, p.name)
}

// gotFile handles one typFile packet from p.
func (n *node) gotFile(p *peer, plain []byte) {
	if len(plain) < 6 {
		return
	}
	kind, id := plain[1], binary.BigEndian.Uint32(plain[2:])
	key := fileKey{p, id}
	f := n.files
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case fileMiss, fileDone:
		ear := f.miss[key]
		if ear == nil {
			return
		}
		if kind == fileDone {
			delete(f.miss, key)
			close(ear)
			return
		}
		want := make([]uint16, 0, (len(plain)-6)/2)
		for b := plain[6:]; len(b) >= 2; b = b[2:] {
			want = append(want, binary.BigEndian.Uint16(b))
		}
		select {
		case ear <- want:
		default:
		}
		return
	}
	in := f.in[key]
	if in == nil {
		in = &inFile{}
		f.in[key] = in
		go n.chaseFile(p, id)
	}
	in.last = time.Now()
	switch kind {
	case fileHead:
		if len(plain) < 12 || in.name != "" {
			return
		}
		count, size := int(binary.BigEndian.Uint16(plain[6:])), int(binary.BigEndian.Uint32(plain[8:]))
		if size > fileMax || count == 0 || count != (size+fileChunk-1)/fileChunk {
			delete(f.in, key)
			return
		}
		in.name, in.size = string(plain[12:]), size
		chunks := make([][]byte, count)
		for seq, c := range in.chunks { // anything that arrived before the head
			if seq < count {
				chunks[seq] = c
			}
		}
		in.chunks = chunks
	case fileData:
		if len(plain) < 8 {
			return
		}
		seq := int(binary.BigEndian.Uint16(plain[6:]))
		if in.name == "" { // no head yet: keep it, the head will size things
			for len(in.chunks) <= seq {
				in.chunks = append(in.chunks, nil)
			}
		}
		if seq >= len(in.chunks) || in.chunks[seq] != nil {
			return
		}
		in.chunks[seq] = append([]byte(nil), plain[8:]...)
		in.got++
	}
	if in.name != "" && in.got == len(in.chunks) {
		delete(f.in, key)
		data := make([]byte, 0, in.size)
		for _, c := range in.chunks {
			data = append(data, c...)
		}
		go func() {
			done := binary.BigEndian.AppendUint32([]byte{typFile, fileDone}, id)
			for i := 0; i < 3; i++ {
				n.sendTo(p, done)
				time.Sleep(100 * time.Millisecond)
			}
			stored, err := f.store(id, in.name, data)
			if err != nil {
				n.system("file from %s: %v", p.name, err)
				return
			}
			n.chat.addFile(p.name, stored, len(data))
			n.cue(cueChat)
			if n.collector {
				p.logsGot.Store(true)
				fmt.Println("got", p.name, "→", stored)
			}
		}()
	}
}

// chaseFile asks p for whatever is still missing whenever the stream goes
// quiet, and forgets the file when p stops sending.
func (n *node) chaseFile(p *peer, id uint32) {
	key := fileKey{p, id}
	for {
		time.Sleep(fileQuiet)
		n.files.mu.Lock()
		in := n.files.in[key]
		if in == nil {
			n.files.mu.Unlock()
			return
		}
		if time.Since(in.last) > fileGrace {
			delete(n.files.in, key)
			n.files.mu.Unlock()
			n.system("file from %s: gave up, %d of %d chunks", p.name, in.got, len(in.chunks))
			return
		}
		var ask []byte
		if time.Since(in.last) >= fileQuiet {
			ask = binary.BigEndian.AppendUint32([]byte{typFile, fileMiss}, id)
			if in.name != "" {
				for seq, c := range in.chunks {
					if c == nil {
						ask = binary.BigEndian.AppendUint16(ask, uint16(seq))
						if len(ask) > 1400 {
							break
						}
					}
				}
			}
		}
		n.files.mu.Unlock()
		if ask != nil {
			n.sendTo(p, ask)
		}
	}
}
