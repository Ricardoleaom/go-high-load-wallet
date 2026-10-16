# Desafio Backend: Processamento Distribuído de Apostas

> Este repositório evolui o desafio entregue em
> [backend-challenge-go](https://github.com/Ricardoleaom/backend-challenge-go)
> (tag `v0.1.0-challenge`) para um serviço focado em alta volumetria.

Serviço em Go que movimenta carteiras de jogadores via HTTP e SQS, com
idempotência persistente, ledger auditável, transactional outbox e
autenticação OAuth2/OIDC via Keycloak. As decisões técnicas estão em
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## Pré-requisitos

- Docker e Docker Compose
- Go 1.27.1 (a mesma versão do `go.mod` e do `Dockerfile`), só para rodar
  os testes fora do container
- `curl` e `jq` para os exemplos

## Subindo o ambiente

```sh
docker compose up --build
```

Ordem de subida: Postgres, migrations (o serviço `migrate` roda uma vez e
sai), Keycloak (importa o realm), LocalStack (cria as filas) e a API.

| Serviço | Endereço |
| --- | --- |
| API | `http://localhost:8080` |
| Keycloak | `http://localhost:8081` (admin/admin) |
| LocalStack (SQS) | `http://localhost:4566` |
| Postgres | `localhost:5432` (app/local_dev_password); outra porta com `POSTGRES_HOST_PORT` se a 5432 já estiver em uso |

Pronto quando `curl localhost:8080/health/ready` responder
`{"status":"ok",...}`. O Keycloak leva uns 20 segundos na primeira
subida; a API espera por ele sozinha.

## Variáveis de ambiente

Valores locais de exemplo em [`.env.example`](.env.example). No Compose
elas já vêm definidas no serviço `app`, apontando para os nomes dos
containers.

| Variável | Obrigatória | Uso |
| --- | --- | --- |
| `DATABASE_URL` | sim | conexão com o Postgres |
| `JWKS_URL` | sim | chaves públicas do Keycloak para validar tokens |
| `SQS_TRANSACTIONS_QUEUE_URL` | sim | fila de entrada `wager-transactions.fifo` |
| `SQS_EVENTS_QUEUE_URL` | sim | fila de saída `wager-events.fifo` |
| `SQS_ENDPOINT` | não | endpoint do LocalStack |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | não | credenciais do SQS (padrão `us-east-1`/`test`/`test`) |
| `HTTP_PORT` | não | porta da API (padrão `8080`) |
| `OUTBOX_PUBLISH_INTERVAL` | não | segundos entre rodadas do publisher do outbox (padrão `2`) |
| `REFERENCE_RETRY_INTERVAL` | não | segundos entre rodadas do worker de referências pendentes (padrão `15`) |

## Filas SQS

O script [`deploy/localstack/init-queues.sh`](deploy/localstack/init-queues.sh)
roda automaticamente quando o LocalStack fica pronto e cria:

| Fila | DLQ | Uso |
| --- | --- | --- |
| `wager-transactions.fifo` | `wager-transactions-dlq.fifo` | operações recebidas |
| `wager-events.fifo` | `wager-events-dlq.fifo` | eventos publicados pelo outbox |

Todas FIFO, visibility timeout de 30s e redrive para a DLQ após 5
recebimentos. Para conferir:

```sh
docker compose exec localstack awslocal sqs list-queues
```

## Migrations

O serviço `migrate` aplica tudo no `docker compose up`. Manualmente:

```sh
MIGRATE="docker run --rm -v $(pwd)/migrations:/migrations --network host migrate/migrate \
  -path=/migrations -database postgres://app:local_dev_password@localhost:5432/wallet?sslmode=disable"

$MIGRATE up        # aplicar todas
$MIGRATE down 1    # reverter a última
$MIGRATE down -all # reverter todas
$MIGRATE version   # versão atual
```

## Identidades de teste (Keycloak)

O realm `wagering` é importado de
[`deploy/keycloak/realm-export.json`](deploy/keycloak/realm-export.json)
com três clients `client_credentials`:

| Client | Secret | Identidade | Acesso |
| --- | --- | --- | --- |
| `provider-a` | `provider-a-secret` | `provider_id=provider-a` | apostas |
| `provider-b` | `provider-b-secret` | `provider_id=provider-b` | apostas |
| `internal-service` | `internal-service-secret` | role `internal` | carteiras |

```sh
token() {
  curl -s -X POST http://localhost:8081/realms/wagering/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
    | jq -r .access_token
}
INTERNAL_TOKEN=$(token internal-service)
PROVIDER_TOKEN=$(token provider-a)
```

O token vale 5 minutos; gere de novo se receber 401.

## Exemplos de chamada

Abrir carteira (`internal-service`):

```sh
curl -s -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H "Content-Type: application/json" \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

Guarde o `id` retornado em `WALLET_ID`.

Enviar uma aposta (provider). Reenviar o mesmo comando devolve o mesmo
resultado com `idempotentReplay: true`:

```sh
curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: provider-a:transaction-123" \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "'"$WALLET_ID"'",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": {"amount": "25.00", "currency": "BRL"}
  }'
```

Para `REFUND` e `ROLLBACK`, acrescente
`"referenceExternalTransactionId": "transaction-123"` ao corpo.

A mesma operação pelo SQS (`MessageGroupId` = carteira):

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" \
  --message-deduplication-id "provider-a:transaction-456" \
  --message-body '{
    "messageId": "msg-456",
    "type": "WagerTransactionRequested",
    "occurredAt": "2026-10-15T12:00:00.000Z",
    "data": {
      "providerId": "provider-a",
      "externalTransactionId": "transaction-456",
      "idempotencyKey": "provider-a:transaction-456",
      "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
      "walletId": "'"$WALLET_ID"'",
      "roundId": "round-987",
      "gameId": "fortune-chimp",
      "kind": "BET",
      "money": {"amount": "25.00", "currency": "BRL"}
    }
  }'
```

Consultas:

```sh
# carteira, ledger paginado e reconciliação (internal-service)
curl -s -H "Authorization: Bearer $INTERNAL_TOKEN" localhost:8080/wallets/$WALLET_ID
curl -s -H "Authorization: Bearer $INTERNAL_TOKEN" "localhost:8080/wallets/$WALLET_ID/ledger?limit=50"
curl -s -X POST -H "Authorization: Bearer $INTERNAL_TOKEN" localhost:8080/wallets/$WALLET_ID/reconciliation

# transação pelo ID externo (provider dono)
curl -s -H "Authorization: Bearer $PROVIDER_TOKEN" \
  localhost:8080/providers/provider-a/wagering/transactions/transaction-123

# eventos publicados pelo outbox
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wager-events.fifo --max-number-of-messages 10

# saúde e métricas
curl -s localhost:8080/health/ready
curl -s localhost:8080/metrics | grep -E "^(wager|outbox|wallet)_"
```

Lista completa de endpoints, códigos HTTP, corpos de erro e
`failureCode`: seção 11 do [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Testes

```sh
go vet ./...
go test ./...
go test -race ./...
```

Sem variáveis de ambiente, rodam só os testes unitários; os de integração
são pulados com uma mensagem dizendo o que falta. Não há build tags.

### Testes de integração

Usam Postgres, Keycloak e LocalStack reais, sem mocks.

```sh
docker compose up -d postgres migrate keycloak localstack

export DATABASE_URL="postgres://app:local_dev_password@localhost:5432/wallet?sslmode=disable"
export JWKS_URL="http://localhost:8081/realms/wagering/protocol/openid-connect/certs"
export SQS_ENDPOINT="http://localhost:4566"
export SQS_TRANSACTIONS_QUEUE_URL="http://localhost:4566/000000000000/wager-transactions.fifo"
export SQS_EVENTS_QUEUE_URL="http://localhost:4566/000000000000/wager-events.fifo"

go test -race ./...
```

Só `DATABASE_URL` já habilita os testes de `internal/infrastructure/postgres`
e `internal/application/...`. `cmd/api` (composição Fx) precisa das
cinco variáveis.

| Cenário do edital | Teste |
| --- | --- |
| 100.00 com duas apostas de 80.00 simultâneas | `TestConcurrentBetsOnSameWallet` |
| Mesma aposta 50 vezes em paralelo | `TestFiftyConcurrentDuplicateSubmissions` |
| Carteiras distintas ao mesmo tempo | `TestDistinctWalletsProcessConcurrently` |
| Três instâncias independentes | `TestConcurrentBetsAcrossThreeIndependentInstances` (e os três acima também usam várias instâncias) |
| Reversão antes da referência | `TestSubmitRefundBeforeReferenceArrivesGoesPendingThenResolves` |
| Worker de referências em várias instâncias | `TestListDuePendingReferencesClaimsEachRowOnce` |
| Conflito de payload na mesma chave | `TestSubmitRejectsSameKeyDifferentPayload` |
| Reconciliação | `TestReconciliationSnapshot` |
| Início e encerramento do Fx | `TestFxAppStartsAndStopsCleanly` |

### Múltiplas instâncias

Cada "instância" nos testes de concorrência é um `Service` com o seu
próprio `*sql.DB` (pool de conexões e memória próprios) disputando as
mesmas linhas no Postgres. O `compose.yaml` sobe uma única API; o
controle de concorrência está no banco, não no processo (seção 4 do
`ARCHITECTURE.md`).

### Simulações de falha (manuais)

Com o ambiente no ar e os tokens gerados:

- **Reinício**: envie operações, rode `docker compose restart app`,
  reenvie a mesma operação (volta como replay, mesmo saldo) e chame a
  reconciliação (`consistent: true`).
- **Postgres fora**: `docker compose stop postgres`. O `/health/ready`
  responde 503, o HTTP responde 500 e uma mensagem enviada ao SQS fica na
  fila. `docker compose start postgres`: a mensagem é processada na
  reentrega, sem duplicar.
- **Reversão fora de ordem**: envie um `REFUND` com
  `referenceExternalTransactionId` de uma aposta que ainda não existe
  (202, `PENDING_REFERENCE`), depois envie a aposta. Em até uns 20
  segundos a consulta do `REFUND` mostra `PROCESSED`.
- **Reentrega no SQS**: mande o mesmo envelope duas vezes com
  `--message-deduplication-id` diferentes. O log mostra
  `duplicate SQS delivery acknowledged from inbox` e o saldo muda uma vez.
- **HTTP e SQS na mesma operação**: envie por HTTP e depois a mesma
  operação pelo SQS com a mesma `idempotencyKey`. O SQS registra
  `idempotentReplay: true` e há um único débito.

## Lint

```sh
golangci-lint run ./...
```

Configuração em [`.golangci.yml`](.golangci.yml).

## Limitações conhecidas

Ver "Limitações e trabalho não concluído" no
[`ARCHITECTURE.md`](ARCHITECTURE.md).
