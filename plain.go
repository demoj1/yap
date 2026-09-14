package main

import "log"

// plainView just logs state changes; stats come from the node itself.
type plainView struct{}

func (plainView) state(st, peer string) { log.Println(st, peer) }
func (plainView) session(*session)      {}
