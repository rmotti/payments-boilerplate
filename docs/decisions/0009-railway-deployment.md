# ADR 0009: Railway como primeiro destino de deploy

- Status: aceito
- Data: 2026-09-05

## Contexto

O projeto precisa de um caminho reproduzivel para publicar a API, executar o
worker e manter PostgreSQL e RabbitMQ. A primeira implantacao deve exigir pouca
operacao sem transformar detalhes da plataforma em regras de negocio.

## Decisao

Railway sera o primeiro destino documentado. Um projeto contera quatro servicos:

- `api`, publico;
- `worker`, privado;
- PostgreSQL, privado;
- RabbitMQ, privado e com volume persistente.

API e worker usam a mesma imagem e comandos distintos. Migrations sao executadas
por um binario operacional antes do deploy da API. A aplicacao aceita `PORT`,
fornecida pela Railway, e usa variaveis de referencia para conexoes privadas.

GitHub Actions executa o CI. O autodeploy da Railway deve aguardar o resultado
do CI, e um workflow posterior verifica a saude do ambiente publicado.

## Consequencias

- O primeiro deploy nao exige manter um cluster proprio.
- API, worker, banco e broker continuam isolados como servicos independentes.
- RabbitMQ precisa de volume e credenciais fortes.
- A aplicacao precisa tolerar dependencias ainda indisponiveis durante startup.
- Outros destinos continuam possiveis porque configuracao e telemetria usam
  interfaces e protocolos portaveis.

