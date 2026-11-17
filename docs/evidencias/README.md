# Evidências - antes da intervenção

Execução própria em **17/11/2026**, na `main`, commit `cbc3c1ee7f595d066d44fa2825c019c74da8bc6d`. Nenhum código, perfil de parceira ou parâmetro de carga foi alterado. Este conjunto registra somente o **antes**; a comparação com a solução será acrescentada após sua implementação.

## Comando, configuração e janela

Na raiz do repositório, com Go, Make e Docker disponíveis:

```powershell
make down
make reproduce
```

O primeiro comando remove o ambiente e seus volumes; o segundo executa `docker compose up -d --build --wait` e o gerador de carga padrão. Não executar novamente antes de exportar as evidências de uma sessão: Jaeger e Prometheus são efêmeros.

Configuração: 10 requisições sequenciais, seguidas de 200 com 50 em voo; cinco riscos distintos; tenant `corretora-a`; timeout do gerador de 30 s; limite da execução da carga de 2 min; nenhum `ARGS`. Esse timeout pertence ao gerador, **não** ao cliente HTTP das parceiras, que continua sem timeout na base.

Ambiente: Windows amd64, Go 1.25.14, Docker Engine 28.4.0 e Compose 2.39.4-desktop.1. As sementes, taxas de falha e latências das parceiras são os defaults do Compose, preservados no snapshot abaixo.

| Intervalo em 17/11/2026 | UTC | Horário local UTC-03 |
| --- | --- | --- |
| Comandos, incluindo reset e builds | 21:19:18 a 21:21:30 | 18:19:18 a 18:21:30 |
| Fase sequencial do gerador | 21:20:46 a 21:21:05 | 18:20:46 a 18:21:05 |
| Fase de carga | 21:21:05 a 21:21:29 | 18:21:05 a 18:21:29 |
| Janela dos exports e do gráfico | 21:20:31 a 21:21:50 | 18:20:31 a 18:21:50 |

O gráfico foi exportado às 21:23:17 UTC, com o ambiente ainda ativo e os dados da execução ainda retidos. O horário exato de cada span está nos JSONs.

## Medições para a coluna "hoje" dos RNFs

Resultados do [relatório completo](antes-reproduce.txt), medidos pelo gerador do próprio repositório:

| Indicador candidato a RNF | Fase sequencial: 10 / 1 em voo | Carga: 200 / 50 em voo | Unidade e método |
| --- | --- | --- | --- |
| Latência p95 de `POST /quotes` | 2,03 | 8,01 | Segundos; percentil empírico nearest-rank das durações observadas pelo loadgen, incluindo erros |
| Taxa de sucesso HTTP | 50%: 5/10 | 60%: 120/200 | Percentual de respostas bem-sucedidas entre as requisições completadas pelo loadgen |
| Vazão total | 0,5 | 8,1 | Requisições/s; requisições completadas, incluindo erros, divididas pela duração da fase |

A fase sequencial durou 18,77 s; a carga durou 24,81 s. Houve cinco respostas 502 na primeira fase e 80 na segunda, todas atribuídas à `partner-flaky`. Sob carga, p50 foi 6,13 s, p99 foi 8,03 s e o máximo foi 8,05 s.

Esses valores são medições, não metas. A escolha de alvos e sua justificativa pertencem ao SAD. Vazão total não significa vazão de cotações bem-sucedidas. O aumento aparente de sucesso entre fases não indica melhora causada pela concorrência: as amostras têm tamanhos diferentes.

Os 210 traces exportados confirmam o total de **125 respostas 200 e 85 respostas 502**; as métricas brutas também totalizam 210 observações no histograma HTTP de entrada.

## Gráfico de p95 do POST /quotes

![p95 do POST /quotes antes da intervenção](antes-p95.png)

**Legenda:** o p95 estimado pelo histograma aumenta ao longo da carga; as linhas tracejadas marcam seu início e fim. A figura foi renderizada a partir da resposta real da API do Prometheus, sem inventar pontos.

Consulta, incluindo todos os status HTTP:

```promql
histogram_quantile(
  0.95,
  sum by (le) (
    rate(http_server_request_duration_seconds_bucket{
      service_name="quotation-api",
      http_request_method="POST",
      http_route="/quotes"
    }[1m])
  )
)
```

Parâmetros fixos para a comparação futura: janela móvel `[1m]`, passo de **5 s**, eixo Y de **0 a 10 s**, eixo X em UTC. A janela desta captura é `2026-11-17T21:20:31Z` a `2026-11-17T21:21:50Z`. Na captura do depois, conservar escala, passo e janela móvel, deslocando o intervalo absoluto para a nova execução.

A interpolação dos buckets do histograma chegou a **9,702 s** nesta janela. Isso **não** substitui os **8,01 s** do relatório: são métodos diferentes, um histograma com buckets amplos e janela móvel versus um percentil empírico da fase inteira. O intervalo entre os buckets relevantes é 7,5 a 10 s. Os atrasos de exportação OTel e de scrape também deslocam a observação; não há amostra finita de p95 antes de 21:21:06 nesta série. A ausência foi deixada como lacuna, não como zero nem como medição reconstruída.

Consulta HTTP equivalente, cuja resposta foi preservada em [JSON](antes-p95-prometheus.json):

```powershell
$query = 'histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{service_name="quotation-api",http_request_method="POST",http_route="/quotes"}[1m])))'
$url = 'http://127.0.0.1:9090/api/v1/query_range?query=' +
    [Uri]::EscapeDataString($query) + '&start=1794950431&end=1794950510&step=5'
Invoke-WebRequest -Uri $url -NoProxy -OutFile 'docs\evidencias\antes-p95-prometheus.json'
```

Os dados persistidos continuam verificáveis depois que a retenção de uma hora expirar. Reconsultar o Prometheus após esse prazo ou após `make down` não recupera a sessão antiga.

## Trace lento: agregação em série

Artefato: [antes-trace-lento.json](antes-trace-lento.json). Trace ID: `47f8c1a200576948664cbce2c2b8ecc5`.

**Legenda:** resposta HTTP 200 em 8.049,881 ms; três chamadas externas em cascata, sem sobreposição.

| Parceira | Início relativo ao span servidor (ms) | Duração (ms) | Intervalo após a chamada anterior (ms) | Status |
| --- | --- | --- | --- | --- |
| partner-slow | 0,067 | 1.699,726 | Não se aplica | 200 |
| partner-flaky | 1.699,850 | 200,977 | 0,057 | 200 |
| partner-degrading | 1.900,857 | 6.148,984 | 0,030 | 200 |

Os intervalos positivos demonstram a execução sequencial. A parceira degradante domina o tempo da requisição sob concorrência, mesmo respondendo com sucesso.

## Trace com erro: terceira parceira não chamada

Artefato: [antes-trace-erro.json](antes-trace-erro.json). Trace ID: `b090e0b0cf0fe47a2d653775cfd1f486`.

**Legenda:** resposta HTTP 502 em 1.666,985 ms após a `partner-flaky` responder 503; não existe span cliente para a `partner-degrading`.

| Parceira | Início relativo ao span servidor (ms) | Duração (ms) | Status |
| --- | --- | --- | --- |
| partner-slow | 0,069 | 1.507,596 | 200 |
| partner-flaky | 1.507,691 | 159,245 | 503 |
| partner-degrading | Não chamada | Não se aplica | Não se aplica |

A resposta da primeira parceira foi descartada pelo fluxo tudo-ou-nada. O JSON tem três spans: servidor e duas chamadas externas; o trace lento tem quatro.

Para inspecionar na sessão ativa do Jaeger: serviço `quotation-api`, operação `POST /quotes`, intervalo UTC acima, ou abrir `/trace/<trace-id>` na UI. Os JSONs selecionados são exports completos, não apenas resumos das tabelas.

## Índice dos artefatos e origem

| Artefato | Comando ou consulta de origem | Legenda |
| --- | --- | --- |
| [antes-reproduce.txt](antes-reproduce.txt) | `make down` seguido de `make reproduce`, stdout e stderr completos | Reset, builds, inicialização e resultados próprios com carga padrão |
| [antes-contexto.json](antes-contexto.json) | `git rev-parse HEAD`, `git branch --show-current`, `go version`, versões Docker/Compose e relógio UTC | Commit, ferramentas, carga e horário dos comandos |
| [antes-compose.json](antes-compose.json) | `docker compose config --format json`, antes da execução | Configuração efetiva preservando os perfis padrão |
| [antes-metricas.prom](antes-metricas.prom) | `GET http://127.0.0.1:8889/metrics`, após a carga | Snapshot textual das métricas genéricas emitidas pela base |
| [antes-p95-prometheus.json](antes-p95-prometheus.json) | `query_range` e PromQL da seção do gráfico | Amostras originais, inclusive o valor `NaN` |
| [antes-p95.png](antes-p95.png) | Renderização da série da consulta PromQL acima | p95 estimado, com escala, janela e consulta na própria imagem |
| [antes-traces.json](antes-traces.json) | `GET /api/traces?service=quotation-api&operation=POST%20%2Fquotes&limit=300&start=1794950431000000&end=1794950510000000` no Jaeger | Os 210 traces completos da execução |
| [antes-trace-lento.json](antes-trace-lento.json) | Seleção do trace HTTP 200 de maior duração no export completo | Cascata de três chamadas sequenciais |
| [antes-trace-erro.json](antes-trace-erro.json) | Seleção pelo trace ID `b090e0b0cf0fe47a2d653775cfd1f486` no export completo | Erro com ausência da terceira chamada |
| [antes-captura.json](antes-captura.json) | Inspeção dos spans por `span.kind`, `server.address` e `http.response.status_code` e da resposta `query_range` | Janela, contagens, parâmetros do gráfico e cronologia dos traces selecionados |

Comando para exportar o conjunto de traces da janela:

```powershell
$url = 'http://127.0.0.1:16686/api/traces?service=quotation-api&operation=POST%20%2Fquotes&limit=300&start=1794950431000000&end=1794950510000000'
Invoke-WebRequest -Uri $url -NoProxy -OutFile 'docs\evidencias\antes-traces.json'
```

Seleção de um trace completo a partir desse arquivo, preservando o envelope de exportação:

```powershell
$all = Get-Content 'docs\evidencias\antes-traces.json' -Raw | ConvertFrom-Json -Depth 50
$trace = $all.data | Where-Object traceID -EQ '47f8c1a200576948664cbce2c2b8ecc5'
@{ data = @($trace); total = 1; limit = 1; offset = 0; errors = $null } |
    ConvertTo-Json -Depth 50 |
    Set-Content 'docs\evidencias\antes-trace-lento.json' -Encoding utf8
```

Para o trace com erro, substituir o ID e o nome do arquivo pelos indicados na tabela. A consulta deve ser executada com o ambiente ativo; após o reset, usar os arquivos já exportados.

Nenhum teste ou mecanismo novo foi introduzido nesta etapa. Não há evidência de breaker, cache ou fallback, pois eles ainda não existem. O hit rate de produção e os alvos de RNF não são inferidos desta carga sintética.