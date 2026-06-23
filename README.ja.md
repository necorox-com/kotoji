# kotoji 🎍

*Read this in [English](./README.md).*

> **AIが作ったWebツールのための、MCPネイティブなセルフホスト型ホスティング。**

あなた（とあなたのAI）が作ったWebツールに、住む場所を。HTML/CSS/JS のフォルダを置けば、きれいなURLが手に入り、ブラウザ上で編集でき、AIは **MCP** 経由でそれを直接読み書きできます。すべてあなた自身のサーバー上で、**git** があらゆる変更を静かにバージョン管理します。

名前は金沢・兼六園の **ことじ灯籠（kotoji-tōrō）** に由来します。*琴柱*（ことじ）は箏（こと）の弦を支える橋（ブリッジ）であり、kotoji もまた、その上に置かれたツールを支え、声を与える存在です。

---

## なぜ kotoji なのか？

いまやAIによって、非エンジニアでも本当に役立つWebツールを数分で作れる時代になりました。しかし、それを*ホスティングする*ことは依然として面倒です。

- SaaS型ビルダー（Val Town、v0、Bolt、Lovable）はプロプライエタリで、セルフホストできません。
- セルフホスト型PaaS（Coolify、Dokploy）は git-push の CI フローを前提としており、ブラウザ内エディタもAI連携もありません。
- **セルフホスト + MCPネイティブなAIアクセス + git を信頼できる唯一の情報源とする仕組み + 非エンジニアにやさしいエディタ** をひとつにまとめたものは、どこにもありませんでした。

kotoji はその空白を埋めます。

## 機能

- **アップロード & 配信** — 静的ファイルの `.zip` を置くだけで、即座にURLが手に入ります。
- **プロジェクトごとのサブドメイン** — `your-tool.hosting.example.com`。どんなアセットパス形式でも動作します。
- **ブラウザ内編集** — 差分ビュー付きの Monaco エディタで、手早く修正できます。
- **MCPネイティブ** — Claude（または任意のMCPクライアント）を自分のマシンから接続し、サイトを `list / read / write / publish` で直接操作できます。
- **プロジェクトをまたぐユーザー単位のMCP/APIトークン** — トークンは*あなた*に属し、あなたがメンバーである全プロジェクトを自動的にカバーします。各サイトでの実効スコープは `トークンのスコープ ∩ そのサイトでのあなたのロール` で、リクエストごとに再評価されます。そのため、メンバーシップを外せばトークンは即座に制限されます。発行・一覧・失効は設定ページで行え、MCPツールは `site`（ハンドル）セレクタを受け取ります。
- **git が信頼できる唯一の情報源** — 保存はすべてコミットになります。履歴・差分・ロールバックが自然に手に入ります。
- **下書き（draft）と公開（published）** — ブランチ上で安全に作業し、ワンアクションで本番へ昇格できます。各ブランチにはそれぞれ専用のプレビューURLが付きます。
- **GUIで設定するGitHubミラー** — 管理者が設定ページで有効化します（org、PAT、Webhookシークレット）。PATは保存時に暗号化（AES-256-GCM）されます。公開のたびにバックアップとしてGitHubへミラープッシュされます（環境変数でも初期設定できますが、DBの設定が優先されます）。
- **インスタンス設定ページ** — 全員が使える単一の `/settings` 画面：管理者向けのGitHubミラー設定、自分用のMCP/APIトークン管理、そしてMCP接続ガイド。
- **初回起動時の管理者セットアップ** — 単一管理者（`password`）モードでは、パスワードを環境変数に埋め込む必要はもうありません。空のままにしておくと、初回起動時の `/auth/setup` 画面で設定でき（bcryptハッシュとしてDBに保存）、パスワード／セットアップで登録したユーザーはインスタンス管理者に昇格します。
- **差し込み可能な認証** — 標準で Google OAuth（内部はOIDC抽象化。自前のIdPも利用可）、単一管理者パスワードモード、お試し用の認証なし dev モードに対応。
- **起動時マイグレーション** — 組み込みの goose マイグレーション（アドバイザリロック付き）が起動時に自動実行されます（`KOTOJI_AUTO_MIGRATE` で切り替え可能）。
- **2つのデプロイモード** — 自前のプロキシを使う（アプリは `Host` ヘッダからプロジェクトを解決するので、Nginx Proxy Manager、Caddy、nginx、Traefik、開発時の素の `*.localhost` の背後で動作します）か、オプトインの Traefik ターンキー・オーバーレイを追加して、ワイルドカードTLSを自動取得する自己完結型のボックスにできます。
- **ことじ灯籠ブランディング** — ファビコンとブランドマークは、SVGで描かれたことじ灯籠です。

## アーキテクチャ

```
                         ┌──── Control plane ─────────────────┐
  Your machine ─MCP/HTTP─▶│  Next.js                          │
  (Claude, ...)          │  · Auth (Google / OIDC)            │
  Browser ───────────────▶│  · Monaco editor / dashboard      │
                         │  · REST API (/upload, ...)         │
                         │  · MCP server                      │
                         │            │  Site Service (DI)    │
                         └────────────┼───────────────────────┘
                                      ▼
                          /data/sites/{uuid}/.git  (1 site = 1 repo)
                            ├ published   ← served in production
                            ├ draft       ← default working branch
                            └ feature-*   ← per-user / AI proposals
                                      │
                         ┌────────────┼──── Data plane ────────┐
                         │            ▼  resolve {name} by Host │
                         │  name.hosting.example.com   → published
                         │  name--branch.hosting.example.com → preview
                         └─────────────────────────────────────┘

  Metadata: PostgreSQL   ·   Deploy: Docker Compose
```

**git は唯一の信頼できる情報源です。** 3つの書き手（Zipアップロード、Monacoエディタ、MCP）はすべて、gitに触れる唯一のコンポーネントである **Site Service** に集約されます。これにより設計はテスト可能になり（インターフェースでgitをモックできる）、バージョン管理は機能ではなく副作用として実現されます。

## クイックスタート（ローカル）

```bash
git clone https://github.com/necorox-com/kotoji
cd kotoji
docker compose up
```

その後 `http://kotoji.localhost:8080` を開きます。`*.localhost` のサブドメインは自動的に `127.0.0.1` に解決されるため、ローカルではDNSやTLSの設定は不要です。

> 詳細なセットアップ・設定・MCP接続ガイドは [`docs/`](./docs) に、デプロイ手順は [`deploy/README.md`](./deploy/README.md) にあります。

## 本番環境

kotoji は `postgres + backend + frontend` の Docker Compose スタックです。ベースの
compose はあえて **プロキシなし** にしているので、次の2つのデプロイモードから選べます。
詳細な手順（DNSレコード、コピペで使えるプロキシ設定、環境変数）は
[`deploy/README.md`](./deploy/README.md) にあります。

### (a) 既存のプロキシの背後で動かす

すでに共有エッジ（Nginx Proxy Manager、Caddy、nginx、Traefik…）を運用しているなら、
ベースの compose を使い、プロキシをバックエンド（`:8080` コントロール、
`:8081` サーブ）とフロントエンド（`:3000`）に向けます。

```bash
docker compose -f deploy/docker-compose.yml up -d
```

コピペで使える NPM と Caddy の設定は [`deploy/npm/README.md`](./deploy/npm/README.md) にあります。

### (b) オールインワン（ターンキー）

TLSを自動取得する自己完結型のボックスが欲しい場合は、オプトインの Traefik
オーバーレイを追加します。Traefik v3 のエッジを同梱し、ワイルドカード証明書を
自動で発行します。

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.edge.yml up -d
```

最初に一度だけ必要なインフラ作業。これ以降、新しいプロジェクトに変更は不要です。

1. **DNS:** `A` レコードを2つ追加します。`your-domain` → サーバーIP **と**
   `*.your-domain` → サーバーIP です。DNSのワイルドカードは1ラベルにのみマッチするため、
   `*.your-domain` はホストされる全サイト（`my-tool.your-domain`）をカバーしますが、
   裸のapex（頂点ドメイン）はカバーしません。両方が必要です。
2. **ACME DNSトークン:** ワイルドカード証明書には **DNS-01 チャレンジ** が必要なので、
   `deploy/.env` に `KOTOJI_ACME_EMAIL` と DNS プロバイダのトークン（Cloudflare なら
   `KOTOJI_CF_DNS_API_TOKEN` など）を設定します。すると Traefik が
   `your-domain` + `*.your-domain` のワイルドカード証明書を自動発行します。

ACME 系の変数を空のままにすると、オーバーレイは素の **HTTP** で配信します。
`hosting.localhost` や初回起動時に便利です。

## ステータス

✅ 実装済み・デプロイ済み（MVP）。スタック全体が出荷され、稼働しています：アップロード／配信、
Monaco エディタ、ブランチごとのプレビュー、draft → publish、ユーザー単位でメンバーシップに
上限が掛かるトークンを備えたMCPサーバー、GUIでのGitHubミラー設定、初回起動時の管理者セットアップ、
起動時マイグレーション、そしてオプトインの Traefik ターンキー・オーバーレイ。APIの形が落ち着くまでは、
粗削りな部分や破壊的変更があり得ます。

## ライセンス

[AGPL-3.0](./LICENSE) © necorox
