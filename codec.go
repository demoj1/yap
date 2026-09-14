package main

import (
	"gopkg.in/hraban/opus.v2"
)

const (
	sampleRate = 48000
	frameSize  = 960 // 20 ms @ 48 kHz
	bitrate    = 96000
)

type encoder struct {
	enc *opus.Encoder
	out []byte
}

func newEncoder() *encoder {
	enc, err := opus.NewEncoder(sampleRate, 1, opus.AppAudio)
	if err != nil {
		panic(err)
	}
	must(enc.SetBitrate(bitrate))
	must(enc.SetComplexity(10))
	must(enc.SetInBandFEC(true))
	must(enc.SetPacketLossPerc(5))
	must(enc.SetDTX(false))
	return &encoder{enc: enc, out: make([]byte, 1500)}
}

func (e *encoder) encode(pcm []int16) []byte {
	n, err := e.enc.Encode(pcm, e.out)
	if err != nil {
		panic(err)
	}
	return e.out[:n]
}

type decoder struct {
	dec *opus.Decoder
	pcm []int16
}

func newDecoder() *decoder {
	dec, err := opus.NewDecoder(sampleRate, 1)
	if err != nil {
		panic(err)
	}
	return &decoder{dec: dec, pcm: make([]int16, frameSize)}
}

func (d *decoder) decode(pkt []byte) []int16 {
	n, err := d.dec.Decode(pkt, d.pcm)
	if err != nil {
		panic(err)
	}
	return d.pcm[:n]
}

// decodeLost conceals one missing frame (Opus PLC).
func (d *decoder) decodeLost() []int16 {
	if err := d.dec.DecodePLC(d.pcm); err != nil {
		panic(err)
	}
	return d.pcm
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
