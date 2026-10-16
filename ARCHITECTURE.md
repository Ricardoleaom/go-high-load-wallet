# ARCHITECTURE.md

## 1. Dinheiro (`internal/domain/money`)

`Money` é imutável e guarda o valor em `int64` unidades mínimas (centavos)
mais a moeda (código ISO 4217, três letras maiúsculas). Nunca passa por
`float32`/`float64`: parsing, aritmética, serialização e persistência
(colunas `BIGINT`) trabalham só com inteiros e strings.

`New(amount, currency)` aceita apenas dígitos, um ponto e exatamente duas
casas decimais (`"25.00"`). Rejeita vazio, sinal (`+`/`-`), notação
científica, `NaN`, `Infinity`, escala diferente de duas casas e qualquer
outro caractere. Nada é arredondado. A única forma equivalente aceita são
zeros à esquerda (`"025.00"` vale `25.00`). Normalização antes do hash de
idempotência: o valor entra no hash reformatado a partir das unidades
mínimas (`DecimalString`), então `"025.00"` e `"25.00"` geram o mesmo
hash.

`Add`, `Subtract`, `Negate` e `Compare` exigem moedas iguais
(`ErrCurrencyMismatch`) e verificam overflow de `int64` explicitamente.
Limite: `9223372036854775807` centavos, ou seja
`92.233.720.368.547.758,07` na unidade da moeda. Valores negativos só
nascem internamente via `Subtract`/`Negate` (diferença da reconciliação,
por exemplo), nunca de uma entrada externa, e o saldo da carteira não pode
ficar negativo (seção 4).

Do banco para o domínio, o valor volta pela mesma validação de `New`,
formatado como `"%d.%02d"` a partir das unidades mínimas.

## 2. Acesso ao banco e transações SQL

Biblioteca: `database/sql` com o driver `github.com/jackc/pgx/v5/stdlib`,
SQL explícito escrito à mão, sem ORM. Migrations versionadas com
`golang-migrate` (`migrations/`, cada uma com `up` e `down`).

Quem decide onde a transação SQL começa e termina é o caso de uso
(`internal/application/...`), não o repositório. Os métodos de escrita dos
repositórios recebem o `*sql.Tx` do chamador (e recusam `nil`), então uma
única transação atravessa carteira, transação, ledger e outbox. O que é
confirmado junto em cada caso:

| Operação | Confirmado no mesmo commit |
| --- | --- |
| Abertura com saldo > 0 | carteira, `OPENING` em `PROCESSED`, crédito no ledger, `WagerTransactionProcessed` e `WalletBalanceChanged` |
| Abertura com saldo zero | só a carteira |
| `BET`/`WIN` processado | transação `PROCESSED`, `UPDATE` condicional da carteira, lançamento, `WagerTransactionProcessed` e `WalletBalanceChanged` |
| `BET` sem saldo ou moeda divergente | a tentativa sofre rollback; em seguida, um novo commit grava a transação `REJECTED` e `WagerTransactionRejected` |
| `LOSS` | transação `PROCESSED` e `WagerTransactionProcessed` (carteira e ledger intocados) |
| `REFUND`/`ROLLBACK` sem referência | transação `PENDING_REFERENCE`, agendamento do retry e `WagerTransactionPendingReference` |
| `REFUND`/`ROLLBACK` aplicado | lock da transação original, reversão `PROCESSED`, carteira, lançamento e os dois eventos |

## 3. WagerTransaction e máquina de estados (`internal/domain/wager`)

Tipos: `OPENING`, `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`. Regras de
valor validadas no construtor: `OPENING`, `BET` e `WIN` exigem valor maior
que zero; `LOSS` exige exatamente `0.00`; `REFUND` e `ROLLBACK` exigem
valor maior que zero e `referenceExternalTransactionId`.

```
PENDING ──> PROCESSED
   │    ──> REJECTED
   │    ──> FAILED
   └──> PENDING_REFERENCE ──> PROCESSED | REJECTED
```

`canTransition()` só permite sair de `PENDING` e `PENDING_REFERENCE`;
`PROCESSED`, `REJECTED` e `FAILED` são terminais e qualquer nova transição
devolve `ErrInvalidTransition`. `New()` sempre cria em `PENDING`;
`Rehydrate()` reconstrói o que veio do banco sem reaplicar movimentação,
transição ou evento, então carregar uma transação para um `GET` ou para um
replay nunca tem efeito colateral.

**`PENDING` nunca é confirmado sozinho.** Todo processamento é síncrono: a
transação nasce em memória em `PENDING` e é gravada já no estado final (ou
em `PENDING_REFERENCE`) no mesmo commit da operação. O edital permite isso
para operações sem dependência ("sem commit intermediário de aceite"). Com
isso, não existe `PENDING` órfão para outra instância retomar; a única
espera durável é `PENDING_REFERENCE`, que tem worker próprio (seção 7).

**Falha transitória e falha permanente.** Falhas de infraestrutura
(Postgres ou SQS fora, timeout, erro inesperado) não viram registro: a
transação SQL sofre rollback, nada é confirmado e o erro volta para quem
chamou. No HTTP, o cliente recebe 500 e pode reenviar com a mesma
`Idempotency-Key` com segurança. No SQS, a mensagem não é removida e volta
para a fila (seção 9). Rejeições de negócio são definitivas e persistidas
como `REJECTED` com `failureCode` (seção 11). O estado `FAILED` existe no
domínio, mas a aplicação não o grava hoje; as falhas que esgotam as
tentativas do SQS ficam registradas na DLQ, não na tabela (ver
limitações).

A transação guarda a referência em dois campos: `reference_external_id` (o
`referenceExternalTransactionId` como veio do provider) e `reference_id` (o
ID interno da original, preenchido só quando a referência é resolvida).

`OPENING` é interno. O handler HTTP rejeita `kind: "OPENING"` com 400 e
`Submit` devolve `ErrOpeningNotAllowed` como segunda barreira, o que cobre
também o SQS. `OPENING` dispensa provider, IDs externos, chave, hash,
rodada e jogo; a constraint `transactions_external_fields_required`
(migration `000008`) exige esses campos para todos os outros tipos, e o
índice único `idx_transactions_single_opening` impede um segundo crédito
inicial na mesma carteira.

## 4. Wallet e concorrência (`internal/domain/wallet`)

A carteira é a raiz do agregado: identidade, jogador, moeda, saldo,
versão e timestamps. `(playerId, currency)` é único
(`idx_wallets_player_currency_unique`); uma segunda abertura para o mesmo
par devolve 409. `version` começa em `1` e só incrementa quando o saldo
muda, então `LOSS` não altera a versão. O schema impõe
`CHECK (balance_minor_units >= 0)`, `CHECK (version >= 1)` e o formato da
moeda.

### Estratégia: atualização atômica condicionada

Em vez de `SELECT ... FOR UPDATE` seguido de `UPDATE`, ou de controle
otimista com retry, cada movimentação é um único `UPDATE`
(`internal/infrastructure/postgres/wallet_writer.go`):

```sql
UPDATE wallets
SET balance_minor_units = balance_minor_units - $1, version = version + 1, ...
WHERE id = $2 AND currency = $3 AND balance_minor_units >= $1 AND version < ...
RETURNING balance_minor_units, version
```

O Postgres trava a linha durante o `UPDATE` e reavalia o `WHERE` com o
valor mais recente, então duas apostas concorrentes nunca leem o mesmo
saldo antigo: não há janela entre ler e escrever, logo não há lost update.
Cada carteira é uma linha e cada `UPDATE` trava só a sua, então carteiras
diferentes avançam em paralelo, sem lock global e sem lock em memória da
aplicação. Como a coordenação está inteira no banco, o resultado é o mesmo
com uma ou com várias instâncias.

Quando o `UPDATE` afeta zero linhas, uma consulta de diagnóstico separa a
causa (carteira inexistente, moeda divergente, saldo insuficiente) para
devolver o `failureCode` certo.

### Demonstração em teste

Todos rodam contra Postgres real e com `-race`
(`internal/application/wager/service_test.go`). Cada `Service` usa o seu
próprio `*sql.DB`, simulando processos independentes com conexões e
memória próprias:

- `TestConcurrentBetsOnSameWallet`: o cenário obrigatório. Carteira de
  100.00, duas apostas de 80.00 ao mesmo tempo, vindas de duas instâncias.
  Resultado: uma `PROCESSED`, uma `REJECTED` com `INSUFFICIENT_BALANCE`,
  saldo final 20.00 e um único débito no ledger.
- `TestConcurrentBetsAcrossThreeIndependentInstances`: três instâncias,
  três apostas de 40.00 sobre 100.00. Exatamente duas processam.
- `TestFiftyConcurrentDuplicateSubmissions`: a mesma aposta 50 vezes,
  distribuída entre três instâncias. Um único débito.
- `TestDistinctWalletsProcessConcurrently`: dez carteiras diferentes
  movimentadas ao mesmo tempo por três instâncias, todas processadas.

O cenário de 100.00/80.00 também foi reproduzido manualmente via HTTP,
com dois `curl` em paralelo contra o ambiente completo do Docker Compose.

## 5. Idempotência

A identidade de uma operação é `(provider_id, idempotency_key)`, com
índice único parcial (`idx_transactions_idempotency_unique`, excluindo
`OPENING`). `(provider_id, external_id)` tem outro índice único parcial,
para que a mesma operação financeira não seja reaplicada com uma chave
diferente. As duas coisas ficam em colunas da tabela `transactions`, então
a idempotência sobrevive ao reinício de todos os processos.

**Hash do payload** (`ComputePayloadHash`,
`internal/application/wager/payload_hash.go`): SHA-256, em hexadecimal, do
JSON de um struct com campos em ordem alfabética fixa: `amount`,
`currency`, `externalId`, `gameId`, `kind`, `playerId`, `providerId`,
`referenceExternalTransactionId` (omitido quando vazio), `roundId`,
`walletId`. `encoding/json` serializa na ordem do struct, o que dá JSON
canônico com chaves ordenadas. Ficam de fora a chave de idempotência e
qualquer metadado de transporte (headers HTTP, envelope SQS,
`messageId`). HTTP e SQS montam o mesmo `SubmitInput`, então a mesma
operação gera o mesmo hash pelas duas entradas.

Fluxo de `Service.Submit`:

1. busca por `(providerId, idempotencyKey)`. Achou e o hash bate: devolve
   o resultado persistido com `idempotentReplay: true`. Hash diferente:
   `IDEMPOTENCY_CONFLICT` (409);
2. busca por `(providerId, externalTransactionId)`. Achou com outra chave:
   `IDEMPOTENCY_KEY_MISMATCH` (409);
3. nada encontrado: processa como operação nova.

**Replay devolve o saldo da época**: para uma transação `PROCESSED`, o
saldo do replay vem do `balance_after` do lançamento dela no ledger, não
do saldo atual da carteira.

**Corrida entre duas primeiras submissões com a mesma chave**: as duas
podem passar pelas buscas acima antes de qualquer commit. O índice único
deixa só um `INSERT` vencer; o perdedor recebe a violação de unicidade,
traduzida para `errIdempotencyRace`, e `Submit` busca de novo e devolve o
resultado do vencedor como replay em vez de erro.

O servidor nunca troca a `Idempotency-Key` recebida por uma calculada; o
header é obrigatório (400 sem ele).

## 6. Ledger (`internal/domain/ledger`)

`ledger.Entry` é imutável e o construtor valida
`balanceAfter = balanceBefore ± amount` conforme a direção (`DEBIT` ou
`CREDIT`). No banco (`ledger_entries`):

- `UNIQUE (transaction_id)`: cada transação gera no máximo um lançamento.
  É mais restrito que o `(walletId, transactionId)` pedido e o implica;
- `CHECK` de valor diferente de zero, saldos antes e depois maiores ou
  iguais a zero e direção válida;
- trigger `ledger_entries_immutable` (função
  `prevent_ledger_entry_mutation`), que recusa qualquer `UPDATE` ou
  `DELETE`. Append-only garantido pelo banco, não por convenção.

`amount_delta_minor_units` guarda o valor com sinal (negativo no débito),
o que torna a reconciliação uma soma simples. `LOSS` e transações
rejeitadas não geram lançamento. Correções financeiras só acontecem com
novos lançamentos (um `ROLLBACK`, por exemplo).

A listagem (`GET /wallets/{id}/ledger`) ordena por `(created_at, id)`, uma
ordem estável mesmo com timestamps iguais, e pagina por cursor opaco
(`base64url` de `created_at|id`), com `limit` padrão 50 e máximo 500.

## 7. Referências pendentes e reversões

`REFUND` e `ROLLBACK` resolvem a referência por
`(providerId, referenceExternalTransactionId)`:

| Situação da referência | Resultado |
| --- | --- |
| Não existe ainda | `PENDING_REFERENCE` (202) |
| Existe, mas está `PENDING_REFERENCE` | `PENDING_REFERENCE`, espera junto |
| Terminou `REJECTED` ou `FAILED` | rejeita na hora com `REFERENCE_NOT_PROCESSED` |
| Diverge em provider, jogador, carteira, rodada, moeda ou valor | `REFERENCE_MISMATCH` |
| Tipo não reversível | `REFERENCE_KIND_NOT_REVERSIBLE` |
| Já tem uma reversão bem-sucedida | `REFERENCE_ALREADY_REVERSED` |
| `PROCESSED` e compatível | aplica a reversão |

`REFUND` só reverte `BET` (credita de volta). `ROLLBACK` reverte `BET`
(credita), `WIN` ou `REFUND` (debita). Reversões parciais não existem: o
valor tem que ser igual ao da original.

**Retry.** A transação pendente recebe `reference_next_attempt_at` com
backoff exponencial (5s, 10s, 20s, 40s... com teto de 5 minutos). O
`RetryWorker` roda a cada `REFERENCE_RETRY_INTERVAL` (15s), pega até 20
pendências vencidas e tenta resolver de novo. Depois de 8 tentativas sem
sucesso (cerca de 15 minutos no total), rejeita com
`REFERENCE_NOT_FOUND_TIMEOUT` e publica `WagerTransactionRejected`. Como o
estado está no banco, a retomada continua depois de um reinício e
qualquer instância pode assumir.

As pendências são reivindicadas de forma atômica
(`ListDuePendingReferences`): um `UPDATE ... WHERE id IN (SELECT ... FOR
UPDATE SKIP LOCKED)` empurra `reference_next_attempt_at` 60 segundos à
frente na mesma instrução que as seleciona. Duas instâncias nunca pegam a
mesma pendência ao mesmo tempo, e se uma morrer no meio, a pendência volta
a ficar disponível quando esse prazo vence. Coberto por
`TestListDuePendingReferencesClaimsEachRowOnce`.

**Uma única reversão bem-sucedida por transação original.** O índice
único parcial `idx_transactions_single_successful_reversal` (migration
`000004`) cobre `(provider_id, reference_id)` onde
`status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK')`. Antes disso, a
aplicação trava a original com `SELECT ... FOR UPDATE` e verifica se já
existe reversão, serializando tentativas concorrentes; o índice é a
garantia do banco caso essa checagem falhe.

**Combinações de `REFUND` e `ROLLBACK` sobre a mesma aposta.** Como o
índice vale para os dois tipos juntos, a aposta original aceita uma única
reversão bem-sucedida, seja `REFUND` ou `ROLLBACK`; a segunda é rejeitada
com `REFERENCE_ALREADY_REVERSED`, o que impede devolver o mesmo débito
duas vezes. Desfazer o próprio `REFUND` continua possível com um
`ROLLBACK` que referencie o `REFUND` (é outra referência), e isso debita
de volta o valor devolvido.

**Reversão sem saldo.** Um `ROLLBACK` de `WIN` ou `REFUND` precisa
debitar. Sem saldo, é rejeitado com `ROLLBACK_INSUFFICIENT_BALANCE`,
código diferente do `INSUFFICIENT_BALANCE` da aposta, e fica auditável na
tabela e no evento de rejeição.

## 8. Inbox e outbox

**Outbox** (`outbox_events`). Todo evento é gravado na mesma transação
SQL da mudança que o originou (seção 2). Se o commit não acontecer, o
evento não existe; se acontecer, o evento está lá. Nada é publicado antes
do commit, porque a publicação só lê o que já foi confirmado.

O `OutboxPublisher` roda a cada `OUTBOX_PUBLISH_INTERVAL` (2s) em todas
as instâncias. Cada rodada abre uma transação, reivindica até 20 eventos
pendentes com `SELECT ... FOR UPDATE SKIP LOCKED`, envia cada um para o
SQS, marca `published_at` e faz commit. Vários publishers em paralelo não
disputam as mesmas linhas: `SKIP LOCKED` pula o que outro já travou.
Falha no envio de um evento não aborta o lote: só ele recebe
`attempts + 1` e um `next_attempt_at` com backoff exponencial (base 2s,
teto 2 minutos). O `id` da linha é o `eventId` do envelope, então toda
republicação preserva o `eventId`.

Recuperação:

- **morte entre o commit do negócio e a publicação**: o evento continua
  com `published_at` nulo e é publicado na próxima rodada, por qualquer
  instância;
- **morte entre o envio ao SQS e a marcação de `published_at`**: a
  transação do publisher cai junto com a conexão, os locks são liberados e
  o evento é reenviado com o mesmo `eventId`, que também é o
  `MessageDeduplicationId` (seção 10). Entrega é at-least-once.

**Inbox** (`inbox_messages`). Unicidade em `(consumer_name, message_id)`,
com o hash do payload, o recebimento e a conclusão (`completed_at`). Uma
reentrega com o mesmo `messageId` e conteúdo diferente é recusada
(`ErrInboxPayloadMismatch`).

**Simplificação assumida**: o registro na inbox e a operação de domínio
não compartilham a mesma transação SQL. São três commits curtos em
sequência: registrar a mensagem, processar (`Submit`, que gerencia as
próprias transações) e marcar `completed_at`. Passar um `*sql.Tx` do
consumidor por toda a camada de aplicação, que hoje decide sozinha quando
abrir e confirmar cada transação (e abre mais de uma nos fluxos de
reversão), não coube no prazo. A garantia contra movimento duplicado não
depende disso: quem impede é o índice único de idempotência da tabela
`transactions` (seção 5). Se o processo morrer entre o `Submit` e o
`completed_at`, a reentrega processa de novo, cai no replay idempotente e
só então marca a inbox. A inbox serve para reconhecer reentregas já
concluídas sem rodar a lógica de negócio e como trilha de auditoria.

## 9. Consumidor SQS

Filas criadas pelo `deploy/localstack/init-queues.sh` (hook `ready.d` do
LocalStack): `wager-transactions.fifo` com `wager-transactions-dlq.fifo`, e
`wager-events.fifo` com `wager-events-dlq.fifo`. Todas FIFO, com
`ContentBasedDeduplication=false`, `VisibilityTimeout=30` e redrive para a
DLQ após `maxReceiveCount=5`.

Na produção de mensagens para `wager-transactions.fifo`:

- `MessageGroupId`: o `walletId`. Mensagens da mesma carteira são
  entregues em ordem e carteiras diferentes andam em paralelo;
- `MessageDeduplicationId`: recomendado usar a `idempotencyKey`. A
  deduplicação do SQS só vale por 5 minutos e não é usada como garantia
  financeira; quem garante é a idempotência do banco.

Fluxo de cada mensagem (long polling de 10s, até 10 por chamada):

1. faz o parse do envelope. A identidade durável é o `messageId` do
   envelope, não o `MessageId` gerado pelo SQS (um produtor que reenvia o
   mesmo envelope recebe outro ID do SQS);
2. registra na inbox com o hash do payload. Se a mensagem já estava
   concluída, só confirma a remoção;
3. chama o mesmo `Service.Submit` do HTTP;
4. marca `completed_at` na inbox;
5. só então remove a mensagem da fila (`DeleteMessage`).

| Resultado | O que acontece |
| --- | --- |
| `PROCESSED`, `REJECTED`, `PENDING_REFERENCE` ou replay | mensagem removida; rejeição de negócio é terminal |
| JSON inválido, sem `messageId`, valor monetário inválido | mensagem não removida; após 5 recebimentos vai para a DLQ |
| Erro de validação, carteira inexistente, conflito de idempotência, hash divergente na inbox | idem: permanente, termina na DLQ |
| Falha transitória (Postgres fora, timeout) | mensagem não removida; volta após o visibility timeout (30s) e é reprocessada com segurança |

Uma falha transitória que dure mais que as 5 tentativas (cerca de 2,5
minutos) também leva a mensagem para a DLQ, de onde pode ser
redirecionada para a fila principal sem risco, graças à idempotência.

**Shutdown.** No `OnStop`, o contexto do consumidor é cancelado: nenhum
`ReceiveMessage` novo começa, a chamada em andamento é interrompida e uma
mensagem no meio do processamento tem a transação SQL abortada em vez de
confirmada. Ela não é removida e volta para a fila depois do visibility
timeout, para reentrega segura.

O caminho HTTP e o SQS disputando a mesma operação foi verificado
manualmente: uma aposta enviada por HTTP e depois pelo SQS com a mesma
chave resulta em um único débito, e o SQS registra o replay
(`idempotentReplay: true`).

## 10. Eventos de saída

Destino: `wager-events.fifo` (com `wager-events-dlq.fifo`).
`MessageGroupId` é o `aggregateId` do evento e `MessageDeduplicationId` é
o `eventId`.

Envelope (versão 1), igual para todos os tipos:

```json
{
  "eventId": "uuid",
  "eventType": "WalletBalanceChanged",
  "aggregateId": "uuid",
  "correlationId": "string",
  "causationId": "string (opcional)",
  "occurredAt": "2026-10-15T12:00:00Z",
  "version": 1,
  "data": {}
}
```

| Evento | Quando | `aggregateId` | `data` |
| --- | --- | --- | --- |
| `WagerTransactionProcessed` | operação concluída, inclusive `LOSS` e `OPENING` | ID da transação | `transactionId`, `walletId`, `kind`, `money` |
| `WalletBalanceChanged` | saldo mudou de fato | ID da carteira | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |
| `WagerTransactionRejected` | rejeição definitiva | ID da transação | `transactionId`, `failureCode` |
| `WagerTransactionPendingReference` | reversão esperando a referência | ID da transação | `transactionId`, `referenceExternalTransactionId` |

`WalletBalanceChanged` usa a carteira como `MessageGroupId`, então as
mudanças de saldo de uma carteira chegam em ordem. Tipo e versão são
definidos pelo construtor de cada evento, timestamps são UTC em RFC 3339,
valores monetários são strings decimais e o payload gravado na outbox é
um snapshot que nunca é alterado.

`correlationId` é o `messageId` quando a operação veio do SQS e o ID da
transação quando veio do HTTP. Contrato de consumo: entrega
at-least-once; o consumidor deve deduplicar por `eventId`, já que a
deduplicação do SQS FIFO só cobre 5 minutos.

## 11. Contrato HTTP

Erros sempre têm o corpo `{"code": "CODIGO", "error": "mensagem"}`.

| Endpoint | Quem pode chamar |
| --- | --- |
| `POST /wallets` | `internal-service` |
| `GET /wallets/{id}` | `internal-service` |
| `GET /wallets/{id}/ledger?cursor=...&limit=50` | `internal-service` |
| `POST /wallets/{id}/reconciliation` | `internal-service` |
| `POST /wagering/transactions` | provider |
| `GET /wagering/transactions/{id}` | provider dono da transação |
| `GET /providers/{providerId}/wagering/transactions/{externalId}` | o próprio provider |
| `GET /health/live`, `GET /health/ready`, `GET /metrics` | público |

Respostas de `POST /wagering/transactions`:

| Situação | HTTP | Corpo |
| --- | --- | --- |
| Processada (inclusive replay) | 200 | `transactionId`, `status: PROCESSED`, `balance`, `idempotentReplay` |
| Aguardando referência | 202 | `status: PENDING_REFERENCE` |
| Rejeição de negócio | 422 | `status: REJECTED`, `failureCode` |
| Entrada inválida | 400 | `INVALID_BODY`, `MISSING_IDEMPOTENCY_KEY`, `INVALID_MONEY`, `VALIDATION_ERROR`, `OPENING_NOT_ALLOWED` |
| Sem token, token inválido ou expirado | 401 | `UNAUTHENTICATED` |
| Token sem permissão, provider do corpo diferente do token | 403 | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Carteira não encontrada (ou de outro jogador) | 404 | `WALLET_NOT_FOUND` |
| Conflito de idempotência | 409 | `IDEMPOTENCY_CONFLICT`, `IDEMPOTENCY_KEY_MISMATCH` |
| Indisponibilidade transitória | 500 | `INTERNAL_ERROR` |

Os endpoints de carteira usam ainda `INVALID_PLAYER_ID`, `INVALID_LIMIT`,
`INVALID_CURSOR` (400), `WALLET_CONFLICT` (409, jogador e moeda já têm
carteira) e devolvem 201 na abertura.

Entradas inválidas (400) e conflitos (409) não gravam nada e podem ser
corrigidas e reenviadas. Um 500 significa que a operação não foi
confirmada ou que o resultado é desconhecido; reenviar com a mesma
`Idempotency-Key` é seguro e devolve o resultado persistido se ela tiver
sido confirmada. Rejeições de negócio (422) são definitivas para aquela
operação: o replay devolve a mesma rejeição, e uma nova tentativa precisa
de uma nova operação.

`failureCode` de rejeições definitivas:

| Código | Significado |
| --- | --- |
| `INSUFFICIENT_BALANCE` | `BET` sem saldo |
| `ROLLBACK_INSUFFICIENT_BALANCE` | `ROLLBACK` que debitaria mais que o saldo |
| `CURRENCY_MISMATCH` | moeda da operação diferente da carteira |
| `REFERENCE_NOT_PROCESSED` | referência terminou sem sucesso |
| `REFERENCE_MISMATCH` | referência diverge em provider, jogador, carteira, rodada, moeda ou valor |
| `REFERENCE_KIND_NOT_REVERSIBLE` | tipo da referência não pode ser revertido por essa operação |
| `REFERENCE_ALREADY_REVERSED` | a referência já tem uma reversão bem-sucedida |
| `REFERENCE_NOT_FOUND_TIMEOUT` | a referência não chegou depois de 8 tentativas |

As consultas de transação devolvem `status` e `failureCode`, o que permite
acompanhar pendências e rejeições.

## 12. Reconciliação

`POST /wallets/{id}/reconciliation` (só `internal-service`) reconstrói o
saldo a partir do ledger, incluindo o crédito de abertura, e compara com
o saldo armazenado. Saldo armazenado, soma de
`amount_delta_minor_units` e quantidade de lançamentos vêm de uma única
instrução SQL (`ReconciliationSnapshot`). O Postgres dá um único snapshot
para cada instrução, então os dois lados da comparação enxergam os mesmos
dados confirmados, mesmo com outras transações movimentando a carteira ao
mesmo tempo.

`difference` é o saldo armazenado menos o reconstruído. Divergência
aparece na resposta (`consistent: false`), num log de nível ERROR e na
métrica `wallet_reconciliation_mismatches_total`. A reconciliação nunca
escreve nada. `TestReconciliationSnapshot` cobre o caso consistente e um
saldo alterado por fora do ledger, que precisa ser reportado.

## 13. Autenticação e autorização

IdP: Keycloak, o recomendado pelo edital. Tem `client_credentials` e
mapeamento de claims customizadas sem código extra, então não foi
preciso emitir tokens próprios. O realm `wagering` é importado no startup
(`deploy/keycloak/realm-export.json`) com três clients:

- `provider-a` e `provider-b`: cada um com um protocol mapper
  `oidc-hardcoded-claim-mapper` que grava a claim `provider_id`;
- `internal-service`: sem `provider_id`, com a role de realm `internal`
  na service account.

**Validação.** `httpapi.Authenticator` valida o JWT via JWKS do Keycloak
(`github.com/MicahParks/keyfunc/v3` com `github.com/golang-jwt/jwt/v5`):
assinatura, expiração e algoritmo. Só `RS256` é aceito
(`jwt.WithValidMethods`), o que bloqueia o ataque de confusão de
algoritmo, em que um token HS256 seria assinado usando a chave pública
como segredo. As chaves são buscadas e renovadas pelo `keyfunc` em
background.

**Permissões.**

- endpoints de carteira exigem a role `internal`;
- endpoints de apostas exigem `provider_id` no token. O provider
  autorizado é sempre o do token: se o `providerId` do corpo for
  diferente, 403. O corpo nunca sobrepõe a identidade autenticada;
- consultas por ID ou por `(providerId, externalId)` só devolvem
  transações do provider autenticado. Por ID, uma transação de outro
  provider responde 404, para não confirmar que ela existe. Replays
  passam pela mesma regra, porque a chave de idempotência é escopada por
  provider;
- requisição rejeitada não toca no banco: a autenticação acontece antes
  de qualquer handler.

**Mensageria.** O acesso às filas é controlado pelas credenciais e
políticas do broker. No consumidor, as validações de domínio continuam
valendo (moeda, valores, `OPENING` proibido, dono da carteira). Ver a
lacuna de identidade do provider no SQS nas limitações.

## 14. Fx, ciclo de vida e shutdown (`cmd/api`)

Módulos (`fx.Module`): `config` (configuração e métricas), `persistence`
(conexão e repositórios), `application` (casos de uso e worker de
referências), `messaging` (cliente SQS, consumidor, publisher e checagem
de readiness) e `http` (autenticador e handler). Tudo entra por
construtores com `fx.Provide`; `fx.Invoke` registra os quatro componentes
com ciclo de vida: servidor HTTP, consumidor SQS, publisher do outbox e
worker de referências.

**Inicialização.** `config.Load` falha se faltar variável obrigatória. A
conexão com o Postgres é aberta e testada com `Ping` antes de subir. O
autenticador tenta buscar o JWKS por até 2 minutos, porque o Keycloak
pode subir depois da aplicação; uma primeira busca que falha conta como
tentativa perdida, nunca como sucesso. Depois do boot, o JWKS é relido a
cada `JWKS_REFRESH_INTERVAL` (padrão 1 hora) enquanto a aplicação estiver
no ar, o que descarta chaves revogadas no IdP. Se algo falhar, o Fx não
inicia.

**Workers.** Os três workers usam `runBackground`
(`cmd/api/lifecycle.go`): no `OnStart` criam um contexto cancelável e
sobem uma goroutine; no `OnStop` cancelam o contexto e esperam a
goroutine terminar, dentro do prazo do shutdown. Se não terminar a tempo,
o `OnStop` devolve erro em vez de seguir em silêncio.

**Shutdown.** Ao receber `SIGTERM`, o Fx executa os `OnStop` na ordem
inversa da inicialização: o servidor HTTP para de aceitar conexões e
espera as que estão em andamento (`http.Server.Shutdown`), os workers
param de buscar trabalho e terminam (seção 9 para o consumidor), e só
depois a conexão com o Postgres é fechada, porque ela foi registrada
antes de todos que a usam.

**Logs.** O logger JSON é configurado no início do `main`, antes do Fx,
e os eventos do próprio Fx usam o mesmo logger em nível DEBUG.

`TestFxAppStartsAndStopsCleanly` (`cmd/api/main_test.go`) monta a
composição inteira contra o ambiente real e confere que `Start` e `Stop`
terminam sem erro, com os três workers saindo de fato.

O domínio (`internal/domain/...`) não importa Fx, HTTP, SQS nem a
biblioteca de banco. Só `cmd/api` depende do Fx.

## 15. Observabilidade

**Logs** em JSON (`log/slog`). Cada operação de aposta gera uma linha
com `transport` (`http` ou `sqs`), `correlationId`, `messageId` (no SQS),
`transactionId`, `walletId`, `providerId`, `status`, `failureCode` e
`idempotentReplay`. Erros saem com os mesmos identificadores. Não são
registrados tokens, segredos, valores monetários nem payloads completos.

**Métricas** Prometheus em `GET /metrics`:

| Métrica | O que mede |
| --- | --- |
| `http_requests_total{path,method,status}` | requisições por rota, método e status; o método é normalizado para impedir explosão de cardinalidade |
| `wager_transactions_total{status}` | resultados por status |
| `wager_idempotent_replays_total` | duplicatas respondidas por replay |
| `wager_concurrency_conflicts_total` | corridas na chave de idempotência |
| `wager_reference_retries_total` | tentativas do worker de referências |
| `outbox_events_published_total` | eventos publicados |
| `outbox_events_publish_failed_total` | falhas de publicação (retries do outbox) |
| `wallet_reconciliation_mismatches_total` | divergências de reconciliação |

**Health checks**: `/health/live` responde se o processo está de pé;
`/health/ready` testa Postgres e SQS e responde 503 se algum falhar. O
corpo diz só `ok` ou `unavailable` para cada dependência; o erro real vai
para o log, para não expor detalhes internos num endpoint público.

## 16. Consistência entre `playerId` e `walletId`

`Service.Submit` carrega a carteira e confere se ela pertence ao
`playerId` informado antes de qualquer movimentação. Sem isso, um
provider autenticado poderia movimentar a carteira de outro jogador
mandando um `walletId` que conseguisse descobrir. Divergência responde
`WALLET_NOT_FOUND` (404), sem distinguir "não existe" de "é de outro
jogador". A checagem fica na camada de aplicação, então vale para HTTP e
SQS.

Não foi implementado um vínculo provider-jogador (algo como uma tabela
`provider_players`), porque o edital não define esse relacionamento.

## 17. Interpretações adotadas

- Processamento síncrono: nenhuma operação confirma um `PENDING`
  intermediário (seção 3).
- Falhas de infraestrutura não viram `FAILED` na tabela; são tratadas como
  transitórias e repetidas pelo cliente HTTP ou pelo SQS (seção 3).
- Valores com zeros à esquerda são aceitos e normalizados antes do hash
  (seção 1); qualquer outra variação de formato é rejeitada.
- `WIN` aceita `referenceExternalTransactionId` como informação, mas não
  resolve nem valida a referência; o edital só exige isso de `REFUND` e
  `ROLLBACK`.
- Uma única reversão bem-sucedida por transação original, somando
  `REFUND` e `ROLLBACK` (seção 7).
- Unicidade do ledger por `transaction_id`, mais restrita que
  `(walletId, transactionId)`.
- No HTTP, sem um ID de correlação de entrada, o `correlationId` é o ID da
  transação.
- Acesso negado a um recurso de outro dono responde 404 nas consultas
  por ID, para não confirmar existência.

## 18. Limitações e trabalho não concluído

- **Inbox e domínio em commits separados** (seção 8). A maior
  simplificação do projeto; a garantia financeira continua no índice de
  idempotência.
- **Retry do SQS com intervalo fixo.** O retry de falhas transitórias usa
  o visibility timeout fixo de 30s, não um backoff exponencial, e não
  separa erros permanentes de transitórios: os dois passam pelas mesmas 5
  tentativas antes da DLQ.
- **Sem testes automatizados** para dois publishers do outbox disputando
  os mesmos eventos, consumidor interrompido entre commit e
  `DeleteMessage`, reinício da aplicação e cenários cruzando HTTP e SQS.
  Os mecanismos existem (`SKIP LOCKED`, remoção só após a conclusão,
  estado todo no banco) e foram verificados manualmente contra o ambiente
  Docker, mas não viraram teste.
- **Sem testes HTTP end-to-end automatizados.** A cobertura automatizada
  está no domínio, na aplicação, nos repositórios e na composição Fx, que
  é onde ficam as garantias financeiras.
- **Métricas ausentes**: contagem de mensagens enviadas à DLQ, histograma
  de latência de processamento e atraso da outbox (idade do evento
  pendente mais antigo). Hoje o atraso só é observável consultando
  `outbox_events`.
- **Outbox sem limite de tentativas.** Um evento que nunca consegue ser
  publicado continua tentando a cada 2 minutos, visível em
  `outbox_events_publish_failed_total`; não vai para uma DLQ própria.
- **Identidade do provider não verificada no SQS.** No HTTP, o JWT prova
  quem é o provider. No consumidor, o `providerId` vem do corpo da
  mensagem: quem pode publicar na fila compartilhada pode se declarar
  qualquer provider. O edital deixa isso para "credenciais e políticas do
  broker", o que exigiria filas ou políticas IAM por provider. O
  LocalStack usado aqui (uma fila, credenciais de teste compartilhadas)
  não modela isso, então ficou documentado em vez de simulado.
- **Múltiplas instâncias no Compose.** O `compose.yaml` sobe uma
  instância da API. A coordenação entre instâncias é demonstrada nos
  testes com `Service`s independentes (seção 4), não com vários
  containers.
- Opcionais não implementados: ledger de partidas dobradas, tracing com
  OpenTelemetry, dashboards e teste de carga.
