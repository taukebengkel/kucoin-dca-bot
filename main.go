package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

/* ====================== CONFIGURATION v35.50 3+3 LAYER DCA ====================== */
const (
	kucoinAPIHost  = "https://api.kucoin.com"
	tradeInterval  = 20 * time.Second 
	statusInterval = 30 * time.Minute 

	maxTotalSlots  = 6     
	fixedTradeAmt  = 11.0  // Fixed $11 per entry for small capital ($70 USDT)
	minTradeUSDT   = 5.0

	// DCA STRATEGY
	dcaTriggerPct  = -7.0  
	targetProfitPct = 0.02  

	// SAFETY SETTINGS
	maxAllowed24hDrop = -20.0
	maxAllowed24hPump = 50.0
	flashCrashBtcDrop = -3.0
)

var (
	dynMinDipPct    = -3.5  
	dynOversoldRSI  = 33.0  
	emergencyPanic  = false
	panicResumeTime time.Time
	lastStatusSent  time.Time 

	apiKey, apiSecret, apiPassphrase string
	discordWebhookURL                string
	liveTrade                        bool
	httpc = &http.Client{Timeout: 15 * time.Second}

	posMu, priceMu, metaMu sync.RWMutex
	positions       = make(map[string]*Position)
	priceRegistry   = make(map[string]PriceTick)
	symbolMetaCache = make(map[string]*SymbolMeta)
)

type Position struct {
	Symbol string; Entry float64; Volume float64; IsDCA bool; CreatedAt time.Time 
}

type PriceTick struct { Price float64; At time.Time }
type SymbolMeta struct { BaseIncrement, PriceIncrement float64 }
type Kline struct { Close float64 }

/* ====================== CORE ENGINE ====================== */

func runHunterLoop() {
	refreshGlobalData()
	if checkEmergencySensors() { return }
	adjustStrategyParams()

	if time.Since(lastStatusSent) >= statusInterval { 
		sendHeartbeatStatus() 
		lastStatusSent = time.Now() 
	}
	
	// 1. MONITORING EXIT & DCA
	posMu.Lock()
	for sym, pos := range positions {
		curr := getLivePrice(sym)
		if curr <= 0 { continue }
		pnl := (curr - pos.Entry) / pos.Entry * 100

		if pnl >= targetProfitPct*100 { 
			go executeExit(sym, pos, "PROFIT")
			delete(positions, sym) 
		} else if !pos.IsDCA && pnl <= dcaTriggerPct {
			go executeDCA(sym, pos)
		}
	}
	posMu.Unlock()

	// 2. SCANNING NEW ENTRY
	posMu.RLock(); pCount := len(positions); posMu.RUnlock()
	if pCount >= 3 { return }

	resp, err := httpc.Get(kucoinAPIHost + "/api/v1/market/allTickers")
	if err != nil { return }
	var out struct { Data struct { Ticker []struct { Symbol, ChangeRate, VolValue string } } }
	_ = json.NewDecoder(resp.Body).Decode(&out); resp.Body.Close()

	for _, t := range out.Data.Ticker {
		if !strings.HasSuffix(t.Symbol, "-USDT") || t.Symbol == "BTC-USDT" { continue }
		changePct, _ := strconv.ParseFloat(t.ChangeRate, 64); changePct *= 100
		vol, _ := strconv.ParseFloat(t.VolValue, 64)

		if changePct < maxAllowed24hDrop || changePct > maxAllowed24hPump { continue }

		if changePct <= dynMinDipPct && vol > 5000000.0 {
			posMu.RLock(); _, active := positions[t.Symbol]; posMu.RUnlock()
			if !active { 
				if safe, price := isDipSafe(t.Symbol); safe { 
					executeEntry(t.Symbol, price)
					break 
				} 
			}
		}
	}
}

/* ====================== EXECUTION LOGIC ====================== */

func executeEntry(symbol string, entry float64) {
	bal, _ := getSpotBalance("USDT")
	if bal < fixedTradeAmt { return }

	id, err := maybePlaceSpotOrder(symbol, "buy", fixedTradeAmt, 0)
	if err == nil && id != "" {
		time.Sleep(2 * time.Second)
		_, ds, df := getOrderDetail(id)
		if ds > 0 {
			posMu.Lock()
			positions[symbol] = &Position{Symbol: symbol, Entry: df/ds, Volume: ds, IsDCA: false, CreatedAt: time.Now()}
			posMu.Unlock()
			info(fmt.Sprintf("🔵 **ENTRY:** %s ($%.2f)", symbol, df))
		}
	}
}

func executeDCA(symbol string, oldPos *Position) {
	bal, _ := getSpotBalance("USDT")
	if bal < fixedTradeAmt { return }

	id, err := maybePlaceSpotOrder(symbol, "buy", fixedTradeAmt, 0)
	if err == nil && id != "" {
		time.Sleep(2 * time.Second)
		_, ds, df := getOrderDetail(id)
		if ds > 0 {
			posMu.Lock()
			newVol := oldPos.Volume + ds
			newEntry := (oldPos.Entry*oldPos.Volume + df) / newVol
			positions[symbol] = &Position{Symbol: symbol, Entry: newEntry, Volume: newVol, IsDCA: true, CreatedAt: time.Now()}
			posMu.Unlock()
			info(fmt.Sprintf("🟡 **DCA:** %s Added. New Avg: %.4f", symbol, newEntry))
		}
	}
}

func executeExit(symbol string, pos *Position, reason string) {
	metaMu.RLock(); meta, ok := symbolMetaCache[symbol]; metaMu.RUnlock()
	if !ok { return }
	size := math.Floor(pos.Volume/meta.BaseIncrement) * meta.BaseIncrement
	curr := getLivePrice(symbol)
	pnl := (curr - pos.Entry) / pos.Entry * 100
	
	_, err := maybePlaceSpotOrder(symbol, "sell", 0, size)
	if err == nil {
		info(fmt.Sprintf("🟢 **SELL:** %s at +%.2f%% (%s)", symbol, pnl, reason))
	}
}

/* --- HELPERS & API --- */

func checkEmergencySensors() bool {
	if emergencyPanic {
		if time.Now().After(panicResumeTime) { emergencyPanic = false; info("🛡️ Resuming...") } else { return true }
	}
	kl, _ := getKlines("BTC-USDT", "5min", 2)
	if len(kl) >= 2 {
		btcChange := (kl[len(kl)-1].Close - kl[0].Close) / kl[0].Close * 100
		if btcChange <= flashCrashBtcDrop {
			emergencyPanic = true; panicResumeTime = time.Now().Add(30 * time.Minute)
			info(fmt.Sprintf("🚨 BTC DROP %.2f%%. Pausing 30m.", btcChange)); return true
		}
	}
	return false
}

func adjustStrategyParams() {
	kl, _ := getKlines("BTC-USDT", "15min", 2)
	if len(kl) < 2 { return }
	btcTrend := (kl[1].Close - kl[0].Close) / kl[0].Close * 100
	metaMu.Lock(); defer metaMu.Unlock()
	if btcTrend < -1.0 { dynMinDipPct, dynOversoldRSI = -5.0, 28.0 
	} else if btcTrend > 0.5 { dynMinDipPct, dynOversoldRSI = -2.5, 38.0 
	} else { dynMinDipPct, dynOversoldRSI = -3.5, 33.0 }
}

func isDipSafe(sym string) (bool, float64) {
	kl, _ := getKlines(sym, "1min", 20)
	if len(kl) < 20 { return false, 0 }
	rsi := calculateRSI(kl, 14)
	metaMu.RLock(); limit := dynOversoldRSI; metaMu.RUnlock()
	if rsi <= limit { return true, kl[len(kl)-1].Close }
	return false, 0
}

func calculateRSI(kl []Kline, period int) float64 {
	if len(kl) < period+1 { return 50.0 }
	var gains, losses float64
	for i := len(kl) - period; i < len(kl); i++ {
		diff := kl[i].Close - kl[i-1].Close
		if diff > 0 { gains += diff } else { losses -= diff }
	}
	if losses == 0 { return 100.0 }
	return 100.0 - (100.0 / (1.0 + (gains / losses)))
}

func getSpotBalance(c string) (float64, error) {
	req, _ := authReq("GET", "/api/v1/accounts", nil); resp, err := httpc.Do(req)
	if err != nil { return 0, err }; defer resp.Body.Close()
	var res struct { Data []struct { Currency, Type, Available string } }
	_ = json.NewDecoder(resp.Body).Decode(&res)
	for _, a := range res.Data { if a.Currency == c && a.Type == "trade" { v, _ := strconv.ParseFloat(a.Available, 64); return v, nil } }
	return 0, nil
}

func maybePlaceSpotOrder(s, side string, funds, size float64) (string, error) {
	if !liveTrade { return "paper", nil }
	m := map[string]string{"clientOid": fmt.Sprintf("%d", time.Now().UnixNano()), "symbol": s, "side": side, "type": "market"}
	if side == "buy" { m["funds"] = strconv.FormatFloat(funds, 'f', 4, 64) } else { m["size"] = strconv.FormatFloat(size, 'f', 8, 64) }
	b, _ := json.Marshal(m); req, _ := authReq("POST", "/api/v1/orders", b); resp, err := httpc.Do(req)
	if err != nil { return "", err }; defer resp.Body.Close()
	var out struct { Data struct { OrderID string } }; _ = json.NewDecoder(resp.Body).Decode(&out); return out.Data.OrderID, nil
}

func getOrderDetail(id string) (string, float64, float64) {
	if !liveTrade { return "done", 1, 1 }
	req, _ := authReq("GET", "/api/v1/orders/"+id, nil); resp, err := httpc.Do(req)
	if err != nil { return "error", 0, 0 }; defer resp.Body.Close()
	var out struct { Data struct { DealSize, DealFunds string } }; _ = json.NewDecoder(resp.Body).Decode(&out)
	ds, _ := strconv.ParseFloat(out.Data.DealSize, 64); df, _ := strconv.ParseFloat(out.Data.DealFunds, 64); return "done", ds, df
}

func getKlines(s, interval string, limit int) ([]Kline, error) {
	url := fmt.Sprintf("%s/api/v1/market/candles?type=%s&symbol=%s", kucoinAPIHost, interval, s)
	resp, err := httpc.Get(url); if err != nil { return nil, err }; defer resp.Body.Close()
	var res struct { Data [][]string }; _ = json.NewDecoder(resp.Body).Decode(&res)
	var klines []Kline
	for i := len(res.Data) - 1; i >= 0; i-- { if len(klines) >= limit { break }; c, _ := strconv.ParseFloat(res.Data[i][2], 64); klines = append(klines, Kline{Close: c}) }
	return klines, nil
}

func info(msg string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
	if discordWebhookURL != "" {
		payload, _ := json.Marshal(map[string]string{"content": msg})
		_, _ = http.Post(discordWebhookURL, "application/json", bytes.NewBuffer(payload))
	}
}

func sendHeartbeatStatus() {
	bal, _ := getSpotBalance("USDT")
	posMu.RLock(); count := len(positions); posMu.RUnlock()
	info(fmt.Sprintf("🏹 **v35.50 DCA** | Bal: %.2f | Pos: %d/3", bal, count))
}

func refreshGlobalData() {
	resp, err := httpc.Get(kucoinAPIHost + "/api/v1/market/allTickers")
	if err != nil { return }; defer resp.Body.Close()
	var out struct { Data struct { Ticker []struct { Symbol, Last string } } }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	priceMu.Lock()
	for _, t := range out.Data.Ticker { p, _ := strconv.ParseFloat(t.Last, 64); priceRegistry[t.Symbol] = PriceTick{Price: p, At: time.Now()} }
	priceMu.Unlock()
}

func getLivePrice(sym string) float64 {
	priceMu.RLock(); defer priceMu.RUnlock(); t, ok := priceRegistry[sym]
	if ok && time.Since(t.At) < 60*time.Second { return t.Price }; return 0
}

func updateSymbolMetaCache() {
	resp, err := httpc.Get(kucoinAPIHost + "/api/v2/symbols")
	if err != nil { return }; defer resp.Body.Close()
	var out struct { Data []struct { Symbol, BaseIncrement, PriceIncrement string } }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	metaMu.Lock()
	for _, d := range out.Data { bi, _ := strconv.ParseFloat(d.BaseIncrement, 64); pi, _ := strconv.ParseFloat(d.PriceIncrement, 64); symbolMetaCache[d.Symbol] = &SymbolMeta{BaseIncrement: bi, PriceIncrement: pi} }
	metaMu.Unlock()
}

func authReq(m, e string, b []byte) (*http.Request, error) {
	t := strconv.FormatInt(time.Now().UnixNano()/1e6, 10)
	h := hmac.New(sha256.New, []byte(apiSecret)); h.Write([]byte(t + m + e + string(b))); s := base64.StdEncoding.EncodeToString(h.Sum(nil))
	h2 := hmac.New(sha256.New, []byte(apiSecret)); h2.Write([]byte(apiPassphrase)); p := base64.StdEncoding.EncodeToString(h2.Sum(nil))
	req, _ := http.NewRequest(m, kucoinAPIHost+e, bytes.NewReader(b))
	req.Header.Set("KC-API-KEY", apiKey); req.Header.Set("KC-API-SIGN", s); req.Header.Set("KC-API-TIMESTAMP", t); req.Header.Set("KC-API-PASSPHRASE", p); req.Header.Set("KC-API-KEY-VERSION", "2"); req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func main() {
	_ = godotenv.Load()
	apiKey, apiSecret, apiPassphrase = os.Getenv("KUCOIN_API_KEY"), os.Getenv("KUCOIN_API_SECRET"), os.Getenv("KUCOIN_API_PASSPHRASE")
	discordWebhookURL, liveTrade = os.Getenv("DISCORD_WEBHOOK_URL"), os.Getenv("LIVE_TRADE") == "true"
	updateSymbolMetaCache(); refreshGlobalData()
	lastStatusSent = time.Now() 
	info("🚀 v35.50 DCA BOT STARTED")
	ticker := time.NewTicker(tradeInterval)
	for range ticker.C { runHunterLoop() }
}
