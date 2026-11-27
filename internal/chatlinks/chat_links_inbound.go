package chatlinks

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/bus"
	"github.com/kayushkin/bus/messages"
	"github.com/nats-io/nats.go"
)

// StartInbound reads messaging.inbound through a durable consumer of
// its own, so a message that arrives while this server restarts is read when
// it is back. A message no active link holds is acknowledged and left; one a
// link holds is acknowledged only once it is kept. It does nothing when chat
// links are off, and an error is a bus that cannot be reached.
func (s *Service) StartInbound(natsURL string) error {
	if s.ChatLinks == nil {
		return nil
	}
	client, err := bus.Connect(bus.Options{URL: natsURL, Name: "llm-bridge-server-chat-links"})
	if err != nil {
		return fmt.Errorf("chat links: NATS %s: %w", natsURL, err)
	}
	if err := client.EnsureStreams(); err != nil {
		return fmt.Errorf("chat links: NATS streams: %w", err)
	}
	_, err = client.JetConsumeNew(messages.MessagingInboundSubject, chatLinksInboundConsumer, func(message *nats.Msg) {
		var inbound Inbound
		if err := json.Unmarshal(message.Data, &inbound); err != nil {
			log.Printf("[chat-links] ERROR: a messaging.inbound message is not JSON, left: %v", err)
			_ = message.Term()
			return
		}
		kept, err := s.KeepInbound(inbound)
		if err != nil {
			log.Printf("[chat-links] ERROR: keep %s: %v; asking for it again", inbound.EventID, err)
			_ = message.NakWithDelay(30 * time.Second)
			return
		}
		_ = message.Ack()
		if kept {
			s.ChatLinks.wake()
		}
	})
	if err != nil {
		return fmt.Errorf("chat links: consume %s: %w", messages.MessagingInboundSubject, err)
	}
	log.Printf("[chat-links] consuming %s as %s", messages.MessagingInboundSubject, chatLinksInboundConsumer)
	return nil
}
