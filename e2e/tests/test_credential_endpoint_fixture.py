# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import ipaddress

from cryptography import x509
from cryptography.hazmat.primitives.asymmetric import padding

from snapshot_e2e import credential_endpoint, s3
from snapshot_e2e.infra import setup

STORE = "http://172.18.0.1:19000"
ENDPOINT = "https://172.18.0.1:19443/credentials"


def test_helm_values_replace_the_secret_with_the_endpoint() -> None:
    values = dict(item.split("=", 1) for item in credential_endpoint.helm_set(STORE, ENDPOINT))
    assert "storage.s3.credentialsSecretRef" not in values
    assert values["storage.type"] == "s3"
    assert values["storage.s3.endpoint"] == STORE
    assert values["storage.s3.credentialEndpoint.url"] == ENDPOINT
    assert values["storage.s3.credentialEndpoint.tokenAudience"] == credential_endpoint.AUDIENCE
    assert values["storage.s3.caBundleConfigMapRef"] == credential_endpoint.CA_CONFIGMAP
    assert values["storage.s3.caBundleConfigMapKey"] == credential_endpoint.CA_KEY


def test_helm_values_pass_through_the_installer_unchanged() -> None:
    lines = credential_endpoint.helm_set(STORE, ENDPOINT)
    assert setup.parse_helm_overrides(None, "\n".join(lines)) == lines


def test_credential_url_is_https_on_the_node_address() -> None:
    assert credential_endpoint.credential_url("10.0.0.7", 19443) == ENDPOINT.replace(
        "172.18.0.1", "10.0.0.7"
    )
    assert credential_endpoint.credential_url("fd00::7", 19443) == "https://[fd00::7]:19443/credentials"


def test_certificates_cover_every_node_address() -> None:
    certificates = credential_endpoint.generate_certificates(["10.0.0.7", "10.0.0.8"], "ns")
    ca = x509.load_pem_x509_certificate(certificates.ca)
    certificate = x509.load_pem_x509_certificate(certificates.certificate)
    assert certificate.issuer == ca.subject
    names = certificate.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
    assert set(names.get_values_for_type(x509.IPAddress)) == {
        ipaddress.ip_address("10.0.0.7"),
        ipaddress.ip_address("10.0.0.8"),
    }
    assert f"{credential_endpoint.NAME}.ns.svc" in names.get_values_for_type(x509.DNSName)
    ca.public_key().verify(
        certificate.signature,
        certificate.tbs_certificate_bytes,
        padding.PKCS1v15(),
        certificate.signature_hash_algorithm,
    )


def test_endpoint_reads_the_store_login_and_serves_on_the_node_network() -> None:
    deployment = credential_endpoint.endpoint_deployment("ns", 19443, "node-a")
    spec = deployment.spec.template.spec
    assert spec.host_network is True
    assert spec.node_name == "node-a"
    assert deployment.spec.strategy.type == "Recreate"
    assert spec.service_account_name == credential_endpoint.NAME
    container = spec.containers[0]
    secrets = {
        item.name: item.value_from.secret_key_ref
        for item in container.env
        if item.value_from is not None
    }
    assert secrets["ACCESS_KEY"].name == s3.CREDENTIALS_SECRET
    assert secrets["ACCESS_KEY"].key == s3.ROOT_USER_KEY
    assert secrets["SECRET_KEY"].key == s3.ROOT_PASSWORD_KEY
    plain = {item.name: item.value for item in container.env if item.value is not None}
    assert plain["TOKEN_AUDIENCE"] == credential_endpoint.AUDIENCE
    assert plain["PORT"] == "19443"
    assert credential_endpoint.SERVER_SCRIPT.name in container.command[-1]
