# KuCoin Auto-DCA Scalper Bot (v35.50) 🚀

A high-performance, automated cryptocurrency trading bot built in Golang, specifically optimized for the KuCoin exchange. This bot uses a sophisticated **3+3 Layering DCA Strategy** with an integrated **Anti-Crash & Anti-Pump protection system**.

## 🌟 Key Features

- **3+3 DCA Strategy**: Manages 3 active coin positions simultaneously with 3 dedicated backup slots for Dollar Cost Averaging (DCA).
- **Anti-Crash Protection**: Automatically filters out assets dropping more than 20% in 24h to avoid "falling knives."
- **Anti-Pump Protection**: Prevents "buying at the top" by ignoring coins that have surged over 50% in 24h.
- **BTC Guardian**: Monitoring Bitcoin's volatility in real-time. If BTC drops significantly, the bot pauses all entries for 30 minutes to protect your capital.
- **Dynamic Strategy**: Automatically adjusts RSI and Dip thresholds based on Bitcoin's 15-minute trend.
- **Discord Integration**: Get instant notifications for every Buy, Sell, and DCA action directly on your Discord server.

## 📈 Trading Logic

| Action | Condition |
| :--- | :--- |
| **First Entry** | RSI < 33 (Dynamic) and 24h Volume > $5M. |
| **DCA Entry** | Triggered if the price drops -7% from the initial entry. |
| **Take Profit** | Fixed at +2.0% (Net Profit) across the entire position. |

## 🛠️ Installation & Setup

1. **Prerequisites**:
   - Install Go (1.18 or higher)
   - A Raspberry Pi or VPS (Ubuntu recommended)
   - KuCoin API Keys (Spot Trading permissions)

2. **Clone the Repository**:
   ```bash
   git clone [https://github.com/YOUR_USERNAME/kucoin-dca-bot.git](https://github.com/YOUR_USERNAME/kucoin-dca-bot.git)
   cd kucoin-dca-bot
