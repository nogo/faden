// Package faden runs agent turns that survive a restart and can move between
// backends: Claude Code, Codex and a llama.cpp loop. Reach for it when a host
// must not lose a conversation to a crash, or must hand a conversation to
// another model.
//
// A session is a thread of events. A turn is one input and everything until
// the backend's final answer; it is stored before it runs, and on open every
// unfinished turn continues. The design is in docs/decisions.md.
package faden
