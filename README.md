# Prumo Cota — processo e entrega

PoC do desafio de Fundamentos de Arquitetura de Solução. **[SAD: decisões, operação, DR e TCO](docs/sad.md)** · **[Índice completo das evidências](docs/evidencias/README.md)** · [Enunciado original](docs/enunciado.md).

O coração da solução é proteger a fronteira com as seguradoras: timeout limita a espera, breaker por parceira/processo evita chamadas quando a falha já é conhecida, e Redis reutiliza somente propostas válidas, isoladas por corretora, parceira e risco. O fallback preserva as propostas sobreviventes e recusa explicitamente quando nenhuma serve, sem calcular prêmio. Isso custa memória, operações Redis, telemetria e complexidade de validade; cache frio ou indisponível volta a comprar consultas, e resposta parcial perde cobertura comercial. A economia depende de recotação dentro do TTL, não do hit rate artificialmente alto da carga curta.

## O que foi entregue

| Estado | Escopo |
| --- | --- |
| Implementado e exercitado | Timeout, Sony gobreaker v2.4.0, cache Redis com go-redis v9.12.1, fallback completo/parcial/recusa, metadados de origem/validade, métricas e eventos OTel, agregação paralela por parceira (bônus D-05), testes determinísticos e integração no boot |
| Medido no laboratório | Baseline próprio, depois serial, nova coleta paralela com a carga padrão e experimento auxiliar separado; sete categorias de evidência anteriores preservadas |
| Proposta, não provisionada/homologada | Hospedagem em cloud sob política de residência brasileira, autenticação, auditoria imutável de cinco anos, alertas/notificações, operação, backups e recuperação geográfica; [SAD, seções 5–7](docs/sad.md#5-implementação) |
| Estimativa, não economia realizada | Pessoal, infraestrutura, auditoria, ensaios de DR e payback condicional com preços hipotéticos; [SAD, seção 8](docs/sad.md#8-tecnologias-custos-e-pessoal-tco) |
| Não implementado | Retry, bulkhead, rate limiting, filas, Kubernetes e limite global de admissões concorrentes |

O módulo e a base foram preservados; mocks, loadgen e perfis das parceiras não foram alterados para produzir o resultado. O enunciado descreve o **baseline**. A revisão de coerência foi concluída.

## Subir e reproduzir

Execute na raiz do checkout. Para o ambiente, são necessários Docker com Compose v2 e Make; Go 1.25 é necessário para os comandos locais de desenvolvimento. O Compose compila os binários, sem conta em provedor cloud. Confirme portas livres e autorização para usar o Docker compartilhado: os containers têm nomes fixos.

```powershell
docker compose up -d --build --wait
docker compose ps
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/healthz'
```

`make up` também sobe o ambiente, mas não espera os healthchecks como o comando acima. **`/healthz` é liveness, não readiness das dependências**; sucesso nele não prova Redis, parceiras ou exportação funcionando. As portas publicadas não estão restritas a loopback nem autenticadas: não exponha este laboratório à Internet.

Exemplo exclusivamente sintético:

```powershell
$body = @{
    driver = @{ document = 'risco-sintetico-01'; birth_year = 1990 }
    vehicle = @{ plate = 'ABC1D23'; model = 'Modelo de teste'; year = 2020; value_cents = 6000000 }
    coverage = 'comprehensive'
} | ConvertTo-Json -Depth 3
$response = Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/quotes' `
    -Headers @{ 'X-Tenant-Id' = 'corretora-a' } -ContentType 'application/json' -Body $body
$response | Select-Object status, cache_status, degraded, unavailable_partners
$response.quotes
```

O resultado depende do cache e do histórico das parceiras/breakers. `Invoke-RestMethod` lança exceção para HTTP 503; nesse caso, inspecione o corpo de erro, não o trate como resposta 200. `X-Tenant-Id` aceita `corretora-a` ou `corretora-b`, mas **não autentica** quem chama.

Para executar a carga entregue sobre o estado atual:

```powershell
make reproduce
```

Esse alvo sobe com `--wait` e executa **10 requisições sequenciais, depois 200 com 50 em voo**, sem precisar de `ARGS`. Não altere mocks, seeds, perfis, carga ou variáveis de configuração ao comparar com as evidências. Confira também overrides do ambiente e de `.env`: os defaults abaixo devem permanecer iguais.

Para uma execução fria comparável, somente em laboratório exclusivo/autorizado e **depois de exportar os dados que queira preservar**:

```powershell
make down
if ($LASTEXITCODE -ne 0) { throw 'Falha no reset; nao iniciar outra coleta.' }
make reproduce
```

**`make down` executa `docker compose down -v`: remove os containers e volumes deste projeto e perde seu estado efêmero.** Reinicia também sequências dos mocks, aprendizado dos breakers e séries/traces em memória. Não é um procedimento de produção. Aguarde exports do SDK e scrapes antes de capturar; salve novas coletas com outros nomes, sem sobrescrever `antes-*`, `depois-*` ou `auxiliar-*`. Comandos, consultas, janelas e legendas usados na entrega estão no [índice das evidências](docs/evidencias/README.md#depois-da-intervenção).

Desenvolvimento local:

```powershell
make fmt
make vet
make test
make smoke
make build
```

Para repetir somente a suíte sem cache e com todos os casos visíveis, preservando a configuração anterior do shell:

```powershell
$previousGoFlags = $env:GOFLAGS
try {
    $env:GOFLAGS = '-count=1 -v'
    make test
} finally {
    $env:GOFLAGS = $previousGoFlags
}
```

### Parâmetros efetivos

| Variável | Default |
| --- | --- |
| `PARTNER_TIMEOUTS` | `partner-slow=2s,partner-flaky=500ms,partner-degrading=1s` |
| `BREAKER_FAILURE_THRESHOLD` / `BREAKER_INTERVAL` | `3` falhas consecutivas / janela fixa `10s` |
| `BREAKER_OPEN_TIMEOUT` / `BREAKER_MAX_REQUESTS` | `5s` / uma admissão em meio aberto |
| `CACHE_TTL` | `300s`; deve ser positivo e no máximo `300s` |
| `REDIS_OPERATION_TIMEOUT` | `20ms` positivos por operação, incluindo pool, conexão, handshake, escrita e leitura |
| `REDIS_ADDR` | `redis:6379` no Compose; `127.0.0.1:6379` no processo local |

As configurações são lidas no boot; mudança exige recriar o processo. O breaker é **compartilhado pelos tenants por parceira/processo**, não entre réplicas. Timeout próprio conta como falha; cancelamento/prazo do consumidor não. Após 5 s aberto, a próxima tentativa elegível admite a sonda: não há probe por timer, e cache hit pode adiar essa tentativa. Três falhas não limitam o número de chamadas já em voo. Detalhes e justificativas: [SAD, seção 4](docs/sad.md#4-detalhamento-da-arquitetura).

## Cache e isolamento

Chave literal, construída em `internal\quotation\cache.go`:

```text
prumo:quotes:v1:<tenant_b64url>:<partner_b64url>:<risk_sha256_hex>
```

Tenant e parceira usam Base64 URL sem padding. O digest é SHA-256 do JSON da struct de requisição normalizada: documento/ano de nascimento, placa/modelo/ano/valor do veículo e cobertura. Remove-se espaço nas pontas de documento, placa, modelo e cobertura; placa vai para maiúsculas, e cobertura ausente vira `comprehensive`. A identidade do tenant vem do cabeçalho, não do corpo; o envelope confirma tenant/parceira antes de aceitar o hit. O hash não torna o conteúdo anônimo.

A expiração absoluta é `quoted_at + min(CACHE_TTL, validade original, 86400s)`, com `quoted_at` anterior à chamada HTTP e `SET PXAT` em milissegundos arredondados para baixo. Hits não renovam prazo; leitura e composição final exigem pelo menos um segundo inteiro restante. A invalidação é por validade/TTL, sem stale ou exclusão administrativa. Reduzir TTL rejeita entradas com prazo maior que o novo limite; aumentá-lo não estende entradas existentes.

Redis conecta sob demanda e sua falha não impede o boot. Erro de GET segue para a parceira protegida; erro de SET não descarta proposta fresca válida. Não há retry, deduplicação de misses, singleflight, persistência Redis ou transação com a parceira.

## Contrato atual do PoC

| HTTP | `status` | Resultado |
| --- | --- | --- |
| 200 | `complete` | Todas as parceiras configuradas têm proposta válida |
| 200 | `partial` | Ao menos uma proposta válida, com ausentes em `unavailable_partners` |
| 503 | `unavailable` | Nenhuma proposta válida: `quotes=[]`, `error=no_valid_quotes` e `action` com orientação à corretora |

O 503 substitui o antigo 502 de indisponibilidade das parceiras nesse fluxo. A ação é: “Solicite uma nova cotação mais tarde ou contate o suporte da corretora.” Não há retry automático. Cada ausente informa `partner` e `reason`: `timeout`, `circuit_open`, `half_open_limit`, `upstream_error`, `invalid_response` ou `expired`, sem mensagens internas ou URLs.

A resposta contém `tenant_id`, `quotes`, `elapsed_ms`, `status`, `cache_status`, `degraded` e `unavailable_partners`; `error`/`action` aparecem na recusa. Cada proposta mantém seguradora, identificador e prêmio originais e informa `source=partner|cache`, `quoted_at`, `expires_at` e `age_seconds`. **`valid_for_seconds` é a validade restante em segundos inteiros**, inclusive na resposta fresca. Não se apresenta proposta vencida; a ordenação por prêmio crescente e o cabeçalho `X-Tenant-Id` de sucesso permanecem.

`degraded=true` indica resposta parcial/indisponível ou falha Redis. `cache_status` é `available` quando os acessos necessários funcionaram, inclusive misses, ou `unavailable` se algum GET/SET falhou. Uma resposta `complete` pode estar degradada por Redis indisponível; uma resposta toda de cache válido pode ser completa e não degradada mesmo com breaker aberto. Permanecem 400/403 de validação e coberturas `comprehensive`/`third_party`. Cancelamento do consumidor aborta a agregação, não é convertido em sucesso parcial; pode chegar ao caminho de erro 502 legado.

## Métricas e atributos

Pipeline reutilizado: API → Collector → Jaeger/Prometheus. Prometheus: `http://127.0.0.1:9090`; Jaeger: `http://127.0.0.1:16686`, serviço `quotation-api`. O SDK exporta a cada 10 s; o Compose raspa a cada 5 s. A API não tem `/metrics`: o Collector expõe as métricas em `:8889`.

| Sinal | Nome real no Prometheus | Atributos |
| --- | --- | --- |
| Transições do breaker | `prumo_breaker_transitions_total` | `partner`, `from`, `to`; estados `closed`, `open`, `half_open` |
| Lookups de cache | `prumo_cache_lookups_total` | `partner`, `result=hit\|miss\|error` |
| Latência HTTP por parceira | `http_client_request_duration_seconds_bucket` / `_sum` / `_count` | `server_address`; hosts distintos das parceiras no Compose |
| Latência da API | `http_server_request_duration_seconds_bucket` / `_sum` / `_count` | Filtros `service_name="quotation-api"`, `http_request_method="POST"`, `http_route="/quotes"`; status em `http_response_status_code` |

Os novos Counters OTel chamam-se `prumo_breaker_transitions` e `prumo_cache_lookups`. Histogramas HTTP são reutilizados: cache/curto-circuito **não** geram observações artificiais de zero segundo. Cada GET gera um lookup; entrada inválida/vencida é miss, erro de GET é error, e erro de SET não adiciona lookup. Não há gauge de estado atual nem contador específico de parcial/recusa ou erro de SET.

Transições por parceira/direção:

```promql
sum by (partner, from, to) (
  increase(prumo_breaker_transitions_total{service_name="quotation-api"}[1m])
)
```

Hit rate de leituras sem erro e taxa de erros Redis, separadamente:

```promql
sum by (partner) (rate(prumo_cache_lookups_total{service_name="quotation-api",result="hit"}[1m]))
/
sum by (partner) (rate(prumo_cache_lookups_total{service_name="quotation-api",result=~"hit|miss"}[1m]))
```

```promql
sum by (partner) (rate(prumo_cache_lookups_total{service_name="quotation-api",result="error"}[1m]))
```

Denominador zero/série ausente/amostras insuficientes não significam 0% ou 100%. Erros ficam fora desse hit rate operacional, mas entram nas oportunidades usadas para avaliar disponibilidade/economia. Latência p95 por parceira:

```promql
histogram_quantile(0.95,
  sum by (server_address, le) (
    rate(http_client_request_duration_seconds_bucket{
      service_name="quotation-api",
      server_address=~"partner-slow|partner-flaky|partner-degrading"
    }[1m])
  )
)
```

No span servidor `POST /quotes`, eventos `quotation.cache_hit` (`partner`, somente propostas sobreviventes), `partner.short_circuit` (`partner`, `reason=circuit_open|half_open_limit`) e `quotation.cache_error` (`partner`, `operation=get|set`, sem mensagem interna). Um trace todo de cache ou curto-circuito não tem spans HTTP das chamadas evitadas. Os novos atributos não incluem tenant, CPF, placa, `quote_id`, risco, hash ou chave de cache.

## Resultados e sete evidências

Comparação própria com mesmos defaults/perfis; carga padrão de 200 requisições/50 em voo:

| Indicador | Antes, 17/11/2026 | Depois serial, 22/11/2026 | Bônus paralelo, 22/11/2026 |
| --- | --- | --- | --- |
| Sucesso útil | 120/200 | 200/200 | 200/200 |
| p95 empírico | 8,01 s | 1,70 s | 1,71 s |
| Vazão total | 8,1 req/s | 114,1 req/s | 98,0 req/s |
| Duração da fase | 24,81 s | 1,75 s | 2,04 s |

RNF-01 a RNF-03 foram atingidos **nessas amostras**, sob a hipótese de uma proposta válida ser útil. Cinco riscos repetidos e aquecimento anterior favorecem cache; não é SLA, capacidade sustentada nem hit rate de produção. Na fase sequencial iniciada com cache frio (dez pedidos, cinco riscos; não são todos misses), p95 foi **2,03 s → 2,04 s → 1,68 s**: o bônus reduziu 17,65% frente ao serial. Não melhorou a fase de carga aquecida: p95 ligeiramente maior, menor vazão e completas/parciais de **189/11 → 160/40**. Nas duas fases juntas, HTTP efetivos subiram **67 → 123**, GETs Redis falhos **31 → 97**, com três falhas de SET na nova coleta. São custos/limites observados, não ganhos ocultados.

O [registro do bônus paralelo](docs/evidencias/README.md#bônus-paralelo-task-16) reúne os novos artefatos, decisões e comparações. O [trace frio](docs/evidencias/paralelo/trace-paralelo.json) prova sobreposição das três chamadas; o [gráfico novo](docs/evidencias/paralelo/p95.png) mantém consulta, janela e escala anteriores. Uma goroutine por parceira executa cache → breaker → HTTP; os resultados só são compostos após a junção. Falha de uma não cancela irmãs; cancelamento do consumidor é propagado a todas, sem desfazer consultas já compradas. Isolamento, validade e ordenação são preservados; não há retry ou bulkhead.

O [índice completo](docs/evidencias/README.md#as-sete-evidências) contém comandos/PromQL, artefatos, janelas e legendas das sete categorias:

| Categoria | Artefatos de entrada |
| --- | --- |
| Relatório antes/depois | [Antes](docs/evidencias/antes-reproduce.txt), [depois](docs/evidencias/depois-reproduce.txt), [comparação](docs/evidencias/depois-comparacao.json) |
| Suíte completa | [make test](docs/evidencias/202611221600-make-test.txt), [cinco comandos](docs/evidencias/202611221600-validacao.txt), [contexto](docs/evidencias/202611221600-contexto.json), [preservação](docs/evidencias/202611221600-preservacao-testes.json) |
| Breaker aberto | [Trace JSON padrão](docs/evidencias/depois-trace-breaker.json): evento de bloqueio, duas propostas de cache, sem chamadas HTTP |
| Resposta de cache | [Trace JSON padrão](docs/evidencias/depois-trace-cache.json): três propostas de cache, sem chamadas HTTP |
| Ciclo do breaker | [Estado reconstruído dos logs](docs/evidencias/auxiliar-breaker-state.png), [Counter](docs/evidencias/auxiliar-breaker-transitions.png), [PromQL/valores](docs/evidencias/auxiliar-breaker-transitions-prometheus.json) |
| Aquecimento do cache | [Gráfico acumulado](docs/evidencias/auxiliar-cache-hit-rate.png), [PromQL/valores](docs/evidencias/auxiliar-cache-hit-rate-cumulative-prometheus.json) |
| p95 do POST /quotes | [Antes PNG](docs/evidencias/antes-p95.png), [depois PNG](docs/evidencias/depois-p95.png), [antes JSON](docs/evidencias/antes-p95-prometheus.json), [depois JSON](docs/evidencias/depois-p95-prometheus.json) |

São 210 traces padrão no depois **serial**, 196 respostas completas e 14 parciais entre as duas fases. O loadgen original não captura corpos; a classificação é reconstruída dos sinais e complementada por testes do contrato. Houve **31 erros de GET Redis** em 630 oportunidades: 561 hits e 38 misses. Hit rate de leituras saudáveis: 93,66%; participação em todas as oportunidades: 89,05%. Nenhum desses percentuais foi transportado para a conta financeira. As sete provas históricas acima não foram substituídas pela nova coleta do bônus.

O [experimento auxiliar de 40 requisições](docs/evidencias/README.md#experimento-auxiliar-separado-da-comparação-padrão) é separado da comparação padrão. Provou aquecimento e `closed→open→half_open→closed`; o meio aberto durou cerca de 187 ms, não foi alongado para aparecer no gráfico. O p95 estimado do histograma (picos 9,702 s/2,007 s) não é o p95 empírico do relatório. Artefatos foram persistidos antes dos resets e do [encerramento somente deste projeto](docs/evidencias/README.md#integridade-e-encerramento).

### Histórico de verificação e limitações reais

Em 22/11/2026, a validação do commit `98e455389cd70da617d6e6912395ef514d61891f` aprovou os cinco comandos de desenvolvimento, com Go 1.25.14: sete pacotes, 96 testes principais/262 testes e subtestes, sem falha, skip ou cache de resultados. A comparação AST preserva os 64 testes originais, normalizando somente o timeout já acrescentado à fixture com parceiras `a`/`b`. Há testes determinísticos de abertura/recuperação do breaker, isolamento/expiração do cache, fallback e boot real. As alterações até a revisão obrigatória foram documentais; o bônus posterior tem [nova validação](docs/evidencias/paralelo/checks.txt): 98 testes principais/264 com subtestes, cinco comandos aprovados, [64 originais preservados](docs/evidencias/paralelo/preservacao-testes.json) e [suíte completa com race detector em Linux](docs/evidencias/paralelo/race.txt). No Windows não havia CGO/compilador C; o detector rodou em build Docker isolado.

Quando o Docker não respondeu, os sinais foram conferidos primeiro com processos Windows isolados em loopback, mesmas versões de SDK/backends, scrape de 1 s, upstream controlado e miniredis: [condições](docs/evidencias/202611221350/verification.json), [coleta 1](docs/evidencias/202611221350/metrics-phase-1.json), [coleta 2](docs/evidencias/202611221350/metrics-phase-2.json), [oito traces Jaeger](docs/evidencias/202611221350/traces.json). Isso não foi apresentado como carga Compose; os auxiliares foram encerrados sem reiniciar serviços compartilhados. A medição padrão posterior usou Docker novamente disponível, no commit `c6c4c27c85608b252017cc4041ec93756e1d1133`.

O prazo Redis de 20 ms produziu falhas reais, não escondidas como misses. A carga é curta, em datas diferentes e sem controlar outros processos do host; export de 10 s/scrape de 5 s limita sua resolução. Não foi comprovada capacidade sustentada, comportamento de produção ou auditoria regulatória. Prometheus retém 1 h, Jaeger usa memória e o SDK amostra tudo; `up=1` dos jobs do Collector não prova telemetria fresca da API. Breakers não coordenam réplicas, misses simultâneos podem amplificar consultas e cache não substitui autenticação nem auditoria.

## Com mais tempo

Validaria com negócio as hipóteses P-01/P-02 de recotação em até 300 s e utilidade de resposta parcial antes de aprovar economia/payback. Repetiria medições com riscos novos, cache frio/quente, perda de Redis e carga sustentada, verificando quotas, validade e custo por consulta; só então revisaria os 20 ms e os demais limiares com novas evidências, preservando o baseline.

Em uma etapa de produção separada do PoC, implementaria identidade vinculada ao tenant, auditoria imutável e seus dois acknowledgements antes da apresentação, persistência/alertas/limites da observabilidade e os ensaios de DR do SAD. Não foram executados nem estão autorizados por este documento. O bônus de paralelização já foi implementado e medido, sem retries ou outros padrões; a piora de pressão/cobertura na carga aquecida exige ensaio sustentado antes de recomendar produção.

Referências de apoio preservadas: [guia do SAD](docs/guia-sad.md), [roteiro de falha](docs/roteiro-cenario-de-falha.md) e [factibilidade dos mocks](docs/smoke-test-factibilidade.md).