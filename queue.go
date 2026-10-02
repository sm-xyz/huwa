package main

import (
	"crypto/rand"
	"math/big"
	"time"
)

// QueueItem menyimpan data antrean pengiriman pesan
type QueueItem struct {
	SessionID   string
	Phone       string
	GroupJID    string
	Text        string
	MediaURL    string
	TypingDelay int // milidetik
	ResultChan  chan error
}

// AntiBanQueue mengelola antrean pesan dengan simulasi perilaku manusia
type AntiBanQueue struct {
	queueChan chan *QueueItem
	manager   *EngineManager
	minTyping int
	maxTyping int
	minInter  int
	maxInter  int
}

func NewAntiBanQueue(mgr *EngineManager, minTyping, maxTyping, minInter, maxInter int) *AntiBanQueue {
	q := &AntiBanQueue{
		queueChan: make(chan *QueueItem, 1000),
		manager:   mgr,
		minTyping: minTyping,
		maxTyping: maxTyping,
		minInter:  minInter,
		maxInter:  maxInter,
	}

	go q.worker()
	return q
}

// Enqueue memasukkan pesan ke dalam antrean aman
func (q *AntiBanQueue) Enqueue(item *QueueItem) {
	q.queueChan <- item
}

// worker memproses pesan satu per satu dengan simulasi mengetik manusia dan jeda dinamis
func (q *AntiBanQueue) worker() {
	for item := range q.queueChan {
		// 1. Hitung durasi mengetik dinamis berdasarkan panjang karakter
		typingDuration := q.calculateTypingDelay(item.Text)
		if item.TypingDelay > 0 {
			typingDuration = time.Duration(item.TypingDelay) * time.Millisecond
		}

		// 2. Kirim sinyal presence 'sedang mengetik...' ke WhatsApp
		_ = q.manager.SendTypingPresence(item.SessionID, item.Phone, item.GroupJID)
		time.Sleep(typingDuration)

		// 3. Kirim pesan sesungguhnya
		_, err := q.manager.SendTextMessage(item.SessionID, item.Phone, item.GroupJID, item.Text)
		if item.ResultChan != nil {
			item.ResultChan <- err
		}

		// 4. Jeda acak antar-pesan (*inter-message jitter delay*) untuk mengelabui deteksi bot
		jitter := q.randomJitter(q.minInter, q.maxInter)
		time.Sleep(jitter)
	}
}

// calculateTypingDelay menghitung durasi mengetik manusia (1.5s s/d 4.0s)
func (q *AntiBanQueue) calculateTypingDelay(text string) time.Duration {
	charCount := len([]rune(text))
	calculated := charCount * 30 // ~30ms per karakter

	if calculated < q.minTyping {
		calculated = q.minTyping
	}
	if calculated > q.maxTyping {
		calculated = q.maxTyping
	}

	return time.Duration(calculated) * time.Millisecond
}

// randomJitter menghasilkan jeda acak di antara min dan max milidetik
func (q *AntiBanQueue) randomJitter(minMs, maxMs int) time.Duration {
	if maxMs <= minMs {
		return time.Duration(minMs) * time.Millisecond
	}
	diff := maxMs - minMs
	nBig, err := rand.Int(rand.Reader, big.NewInt(int64(diff)))
	if err != nil {
		return time.Duration(minMs) * time.Millisecond
	}
	randomMs := minMs + int(nBig.Int64())
	return time.Duration(randomMs) * time.Millisecond
}
