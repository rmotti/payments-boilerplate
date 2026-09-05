# ADR 0004: Apache License 2.0

- Status: aceito
- Data: 2026-09-05

## Contexto

O projeto pretende ser reutilizado por pessoas desenvolvedoras e empresas em
sistemas próprios, inclusive comerciais e proprietários. Sem uma licença
explícita, o código público não concede as permissões necessárias para esse uso.

Precisamos reduzir o atrito de adoção sem obrigar integradores a publicar seus
sistemas completos. Também é desejável que contribuições tragam uma concessão
explícita das patentes que necessariamente incidam sobre elas.

## Decisão

Código, documentação e exemplos do Payments Boilerplate serão licenciados sob a
Apache License, Version 2.0, identificador SPDX `Apache-2.0`.

Contribuições intencionalmente submetidas para inclusão no projeto serão
recebidas sob a mesma licença, salvo declaração explícita em contrário, conforme
previsto na seção 5.

Um arquivo `NOTICE` será criado apenas quando existirem avisos de atribuição que
precisem ser distribuídos. O projeto não adicionará um arquivo vazio nem
atribuições presumidas.

## Consequências positivas

- Uso, modificação e distribuição comerciais são permitidos.
- Integradores podem manter seus sistemas proprietários.
- Contribuidores concedem direitos autorais e uma licença explícita de patentes
  dentro dos limites definidos pelo texto.
- O software é fornecido sem garantias ou condições, nos limites legais.
- A licença é amplamente reconhecida por ferramentas de compliance.

## Limitações

- Modificações não precisam ser publicadas de volta ao projeto.
- Terceiros podem oferecer comercialmente versões derivadas.
- Avisos aplicáveis e uma cópia da licença precisam ser preservados na
  redistribuição.
- A licença não concede direito sobre nomes e marcas além do necessário para
  descrever a origem do trabalho.
- A licença não substitui avaliação jurídica, conformidade regulatória ou
  revisão das licenças das dependências.

## Alternativas rejeitadas

- MIT: seria mais curta, mas não contém uma concessão explícita de patentes.
- GPL-3.0: imporia copyleft sobre determinadas distribuições e aumentaria atrito
  para integradores proprietários.
- AGPL-3.0: exigiria disponibilização do código correspondente em determinados
  usos pela rede, contrariando o objetivo de adoção permissiva.
- Ausência de licença: impediria que o repositório cumprisse sua proposta de ser
  realmente open source e reutilizável.

