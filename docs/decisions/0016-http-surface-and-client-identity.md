# ADR 0016: fechamento da superfície HTTP, documentação e identidade do cliente

- Status: aceito
- Data: 2026-09-07
- Implementação: aplicada na entrega E6 da Fase 4

## Contexto

A superfície HTTP publicada até aqui é mais larga do que o modelo de acesso do
[ADR 0010](0010-route-access-model.md) pretende. Três lacunas concretas:

**A documentação é pública em qualquer ambiente.** O runtime da API passava
`DocsEnabled: true` fixo. Uma implantação em produção publicava Swagger UI e o
contrato completo sem nenhuma credencial, incluindo os endpoints operacionais
de inspeção e reprocessamento. O contrato não é secreto, mas publicá-lo por
padrão entrega ao atacante o mapa exato da instalação e uma UI pronta para
disparar requisições contra ela.

**Nenhuma resposta carrega header de segurança.** Sem `X-Content-Type-Options`,
o navegador pode inferir o tipo de um corpo e tratar como HTML algo que não é.
Sem `Referrer-Policy`, um identificador presente na URL vaza para terceiros.
Sem política de cache explícita, um intermediário decide sozinho o que guardar
de uma resposta autenticada. E a página do Swagger executa JavaScript sem
nenhuma restrição sobre de onde ele pode vir.

**Não existe noção de quem é o cliente.** O rate limiting da E7 precisa de uma
identidade por requisição. A saída óbvia, ler `X-Forwarded-For`, é a forma
clássica de construir um limitador que qualquer pessoa contorna: quem chega
direto pela internet escolhe o próprio header e, com ele, o próprio balde.

Há ainda uma quarta lacuna, menor e mais silenciosa. O worker registrava o
strict server inteiro no listener de health e devolvia `401` ou `501` nas
operações que não serve. O processo anuncia assim uma superfície que não tem,
e cada operação futura do contrato passa a existir automaticamente nele.

## Decisão

### Documentação por opt-in, e autenticada fora de development

`DOCS_ENABLED` tem default `false` em todos os ambientes. Sem opt-in, nenhuma
rota de documentação é registrada: `/docs`, `/docs/` e `/openapi.yaml`
respondem exatamente como qualquer caminho inexistente.

Com opt-in, o ambiente decide o resto:

| `APP_ENV` | `DOCS_ENABLED` | `/docs`, `/docs/` e `/openapi.yaml` |
| --- | --- | --- |
| qualquer um | `false` | `404`, indistinguível de rota inexistente |
| `development` | `true` | servidos sem credencial |
| qualquer outro | `true` | exigem `X-API-Key` válida; sem ela, `401` |

`.env.example` e o Compose de desenvolvimento ligam a documentação
explicitamente. O default seguro vale para quem implanta; a experiência local
continua a de abrir o navegador e usar o Swagger UI.

Habilitar documentação fora de development registra um warning no startup. Ele
nomeia o ambiente e nunca a chave.

A autenticação da documentação usa a mesma `X-API-Key` das rotas de negócio.
Ela é decidida antes do método e antes do redirect, de modo que `/docs` não
revela por um `301` que a rota existe, nem um `405` revela os métodos aceitos.
Não há credencial própria para documentação: mais um segredo para rotacionar
sem ganho de segurança sobre a chave que já protege as operações reais.

### Headers de segurança em toda resposta

Todas as respostas, incluindo `404` do mux e o `500` do recovery, recebem:

| Header | Valor | Motivo |
| --- | --- | --- |
| `X-Content-Type-Options` | `nosniff` | o corpo é lido como o tipo declarado |
| `Referrer-Policy` | `no-referrer` | nenhum identificador da URL vaza |
| `Cache-Control` | `no-store` | toda resposta é autenticada, um erro correlacionado ou uma leitura de readiness |
| `X-Frame-Options` | `DENY` | complementa `frame-ancestors` em clientes antigos |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'` | um corpo JSON ou YAML não carrega nada |

A página do Swagger é a única exceção e recebe uma política própria:

```text
default-src 'none'; script-src 'self' '<hash de cada script inline>';
style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self';
connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
```

Cada diretiva corresponde a um recurso que a página realmente usa, verificado
contra os arquivos que o `swgui` serve, e não a uma suposição sobre ela. Os
dois scripts inline do template são liberados por hash, calculado uma vez no
startup a partir da própria página renderizada, em vez de `'unsafe-inline'`.
O `'unsafe-inline'` permanece necessário apenas em `style-src`, porque o bundle
estiliza elementos por atributo. O CSS embute imagens como `data:`, o que
explica `img-src`; ele não carrega fontes nem importa folhas externas.
`connect-src 'self'` cobre o carregamento do contrato e o "Try it out" contra a
própria origem. Um teste exercita o handler real e falha se uma atualização do
`swgui` passar a exigir algo que a política não concede.

Uma resposta de erro escrita depois que um handler relaxou a política restaura
os valores estritos, para que a página não empreste sua permissividade a um
corpo JSON que ela nunca pretendeu servir.

### CORS permanece desligado

A API é server-to-server. O `X-API-Key` pertence ao backend do integrador,
nunca ao navegador do consumidor. Emitir `Access-Control-Allow-Origin` por
padrão convidaria justamente o uso que o [ADR 0010](0010-route-access-model.md)
recusa. Nenhum header de CORS é emitido, e um teste falha se algum aparecer.

### `X-Forwarded-For` só de peers confiáveis

`TRUSTED_PROXY_CIDRS` lista as redes cujo `X-Forwarded-For` é acreditado. Ela é
parseada e validada no startup: cada entrada precisa ser um bloco CIDR, e um
endereço isolado é recusado, porque um engano de digitação não pode confiar
numa rede inteira em silêncio.

A resolução é única para todo o processo e vive numa peça reutilizável, para
que o rate limiting da E7 e qualquer outro consumidor concordem sobre quem
chamou em vez de cada um ler o header por conta própria:

- sem CIDR configurado, o cliente é sempre o peer TCP;
- se o peer não pertence a nenhum CIDR confiável, o header é ignorado, então
  quem chega direto da internet não escolhe a própria identidade;
- se o peer é confiável, a cadeia é lida da direita para a esquerda, pulando os
  proxies conhecidos, até o primeiro salto pelo qual nenhum deles responde;
- uma entrada impossível de parsear atribui a requisição ao proxy, porque um
  proxy confiável não escreve lixo: quem escreveu foi quem está atrás dele.

Todo endereço devolvido é canônico. IPv6 mapeado em IPv4 é desmapeado e zonas
são descartadas, de modo que um mesmo cliente não produza duas grafias. Para
agrupamento, IPv4 conta por endereço e IPv6 por `/64`, porque um único host
costuma dispor do prefixo inteiro e contar por endereço permitiria espalhar
requisições por bilhões de chaves.

### O worker serve apenas health

O worker deixa de registrar o strict server completo. Seu mux tem uma única
rota, `GET /health`. As demais operações do contrato não existem naquele
processo: respondem `404`, como qualquer caminho não registrado, em vez de
`401` ou `501`. Uma operação nova do contrato passa a exigir uma decisão
explícita para aparecer no worker, em vez de aparecer sozinha.

## Alternativas consideradas

**Servir a documentação em uma porta separada.** Resolveria a exposição sem
autenticação, mas exigiria um segundo listener, um segundo healthcheck e uma
configuração de rede por plataforma. A política por ambiente entrega o mesmo
resultado sem nada disso.

**Basic auth própria para a documentação.** Adiciona um segredo para gerar,
distribuir e rotacionar sem proteger nada que a `X-API-Key` já não proteja.

**`'unsafe-inline'` em `script-src`.** É o caminho curto e anula boa parte do
valor da política: qualquer script injetado na página passaria a executar. O
hash calculado no startup custa uma renderização e preserva a garantia.

**Confiar em `X-Forwarded-For` sempre que ele existir.** É o comportamento
padrão de muitos frameworks e torna qualquer contagem por IP decorativa.

**Manter o strict server no worker respondendo `501`.** Preserva um detalhe de
implementação como se fosse contrato e faz o processo anunciar uma superfície
que ele não tem.

## Consequências

- Uma implantação que dependia da documentação pública precisa definir
  `DOCS_ENABLED=true` e passar a apresentar `X-API-Key` para lê-la.
- O contrato HTTP não muda. Nenhuma operação, status ou header do OpenAPI é
  afetado: as rotas de documentação sempre estiveram fora dele.
- O rate limiting da E7 recebe pronta a identidade do cliente, sem precisar
  reabrir a discussão sobre proxies.
- Uma atualização do `swgui` que passe a exigir um recurso novo quebra um teste
  em vez de quebrar silenciosamente a página no navegador.
- Quem implanta atrás de um proxy precisa configurar `TRUSTED_PROXY_CIDRS`, ou
  todo tráfego será atribuído ao endereço do proxy.
