# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""A credential endpoint for installing Snapshot against the e2e S3 store
without a credentials Secret.

The endpoint speaks the container-credentials protocol the chart's
``storage.s3.credentialEndpoint`` expects: PageBroker and the operator present
their projected service-account token, the endpoint checks it with a
TokenReview, and answers with the store's keys as short-lived credentials. It
runs on the store's node, on the node network like the store, because the agent
pod cannot resolve cluster service names, with a certificate for that node's
address from a CA generated per run; the CA goes to the chart as
``storage.s3.caBundleConfigMapRef``.

    python -m snapshot_e2e.credential_endpoint deploy   # the endpoint, then chart values

Run ``snapshot_e2e.s3 deploy`` first: the endpoint reads the store's keys from
the Secret that creates. The printed values replace the store's
``credentialsSecretRef`` with the endpoint settings.
"""

from __future__ import annotations

import argparse
import datetime
import ipaddress
import os
import sys
import time
from dataclasses import dataclass
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID
from kubernetes import client, config

from snapshot_e2e import s3

NAME = "snapshot-s3-credentials"
AUDIENCE = NAME
CA_CONFIGMAP = "snapshot-s3-ca"
CA_KEY = "ca.crt"
APP_CONFIGMAP = f"{NAME}-app"
TLS_SECRET = f"{NAME}-tls"
TOKEN_REVIEW_ROLE = f"{NAME}-tokenreview"
IMAGE = (
    "docker.io/library/python:3.12-slim"
    "@sha256:a0e2dd78cc679ddfffea960fe45ecfcff9ac2f68d67917a0edfcde5c2100151b"
)
DEFAULT_PORT = 19443
PORT_ENV = "SNAPSHOT_E2E_S3_CREDENTIAL_PORT"
CREDENTIAL_LIFETIME_SECONDS = 900
READY_TIMEOUT_SECONDS = 300
SERVER_SCRIPT = Path(__file__).with_name("credential_endpoint_server.py")
ISSUED = "issued credentials to"


@dataclass(frozen=True)
class Certificates:
    ca: bytes
    certificate: bytes
    key: bytes


def port_from_env() -> int:
    return int(os.environ.get(PORT_ENV, DEFAULT_PORT))


def helm_set(store_endpoint: str, credential_url: str) -> list[str]:
    """The store's chart values with the endpoint in place of the Secret."""
    values = [
        item
        for item in s3.helm_set(store_endpoint)
        if not item.startswith("storage.s3.credentialsSecretRef=")
    ]
    return values + [
        f"storage.s3.credentialEndpoint.url={credential_url}",
        f"storage.s3.credentialEndpoint.tokenAudience={AUDIENCE}",
        f"storage.s3.caBundleConfigMapRef={CA_CONFIGMAP}",
        f"storage.s3.caBundleConfigMapKey={CA_KEY}",
    ]


def credential_url(host_ip: str, port: int = DEFAULT_PORT) -> str:
    return s3.host_endpoint(host_ip, port).replace("http://", "https://", 1) + "/credentials"


def store_placement(namespace: str) -> tuple[str, str]:
    """The node the store runs on and that node's address on the host network.

    Taken from the store pod rather than the Node objects: inside a vCluster the
    nodes carry virtual addresses, while a pod's hostIP is the real one.
    """
    pods = client.CoreV1Api().list_namespaced_pod(
        namespace, label_selector=f"app.kubernetes.io/name={s3.STORE_NAME}"
    )
    for pod in pods.items:
        if pod.spec.node_name and pod.status.host_ip:
            return pod.spec.node_name, pod.status.host_ip
    raise LookupError(f"no {s3.STORE_NAME} pod with a node in {namespace}")


def generate_certificates(addresses: list[str], namespace: str) -> Certificates:
    """A CA and a server certificate valid on every node address, for one week."""
    now = datetime.datetime.now(datetime.timezone.utc)
    not_before = now - datetime.timedelta(minutes=5)
    not_after = now + datetime.timedelta(days=7)
    ca_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, f"{NAME}-ca")])
    ca = (
        x509.CertificateBuilder()
        .subject_name(ca_name)
        .issuer_name(ca_name)
        .public_key(ca_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(not_before)
        .not_valid_after(not_after)
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(ca_key, hashes.SHA256())
    )
    server_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    names: list[x509.GeneralName] = [
        x509.DNSName(NAME),
        x509.DNSName(f"{NAME}.{namespace}.svc"),
    ]
    names += [x509.IPAddress(ipaddress.ip_address(address)) for address in addresses]
    certificate = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, NAME)]))
        .issuer_name(ca_name)
        .public_key(server_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(not_before)
        .not_valid_after(not_after)
        .add_extension(x509.SubjectAlternativeName(names), critical=False)
        .add_extension(x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH]), critical=False)
        .sign(ca_key, hashes.SHA256())
    )
    return Certificates(
        ca=ca.public_bytes(serialization.Encoding.PEM),
        certificate=certificate.public_bytes(serialization.Encoding.PEM),
        key=server_key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        ),
    )


def endpoint_deployment(namespace: str, port: int, node_name: str) -> client.V1Deployment:
    labels = {"app.kubernetes.io/name": NAME}
    keys = [
        client.V1EnvVar(
            name=name,
            value_from=client.V1EnvVarSource(
                secret_key_ref=client.V1SecretKeySelector(
                    name=s3.CREDENTIALS_SECRET, key=key
                )
            ),
        )
        for name, key in (
            ("ACCESS_KEY", s3.ROOT_USER_KEY),
            ("SECRET_KEY", s3.ROOT_PASSWORD_KEY),
        )
    ]
    settings = [
        client.V1EnvVar(name="TOKEN_AUDIENCE", value=AUDIENCE),
        client.V1EnvVar(name="PORT", value=str(port)),
        client.V1EnvVar(name="LIFETIME_SECONDS", value=str(CREDENTIAL_LIFETIME_SECONDS)),
    ]
    container = client.V1Container(
        name="endpoint",
        image=IMAGE,
        command=["python3", "-u", "/app/credential_endpoint_server.py"],
        env=keys + settings,
        ports=[client.V1ContainerPort(container_port=port)],
        readiness_probe=client.V1Probe(
            tcp_socket=client.V1TCPSocketAction(port=port), period_seconds=2
        ),
        resources=client.V1ResourceRequirements(requests={"cpu": "50m", "memory": "64Mi"}),
        volume_mounts=[
            client.V1VolumeMount(name="app", mount_path="/app", read_only=True),
            client.V1VolumeMount(name="tls", mount_path="/tls", read_only=True),
        ],
    )
    return client.V1Deployment(
        metadata=client.V1ObjectMeta(name=NAME, namespace=namespace, labels=labels),
        spec=client.V1DeploymentSpec(
            replicas=1,
            strategy=client.V1DeploymentStrategy(type="Recreate"),
            selector=client.V1LabelSelector(match_labels=labels),
            template=client.V1PodTemplateSpec(
                metadata=client.V1ObjectMeta(labels=labels),
                spec=client.V1PodSpec(
                    service_account_name=NAME,
                    node_name=node_name,
                    host_network=True,
                    dns_policy="ClusterFirstWithHostNet",
                    containers=[container],
                    volumes=[
                        client.V1Volume(
                            name="app",
                            config_map=client.V1ConfigMapVolumeSource(name=APP_CONFIGMAP),
                        ),
                        client.V1Volume(
                            name="tls",
                            secret=client.V1SecretVolumeSource(secret_name=TLS_SECRET),
                        ),
                    ],
                ),
            ),
        ),
    )


def _create_or_replace(create, replace, *args) -> None:
    try:
        create(*args)
    except client.ApiException as exc:
        if exc.status != 409:
            raise
        replace(*args)


def apply_token_review_access(namespace: str) -> None:
    core = client.CoreV1Api()
    rbac = client.RbacAuthorizationV1Api()
    try:
        core.create_namespaced_service_account(
            namespace, client.V1ServiceAccount(metadata=client.V1ObjectMeta(name=NAME))
        )
    except client.ApiException as exc:
        if exc.status != 409:
            raise
    role = client.V1ClusterRole(
        metadata=client.V1ObjectMeta(name=TOKEN_REVIEW_ROLE),
        rules=[
            client.V1PolicyRule(
                api_groups=["authentication.k8s.io"],
                resources=["tokenreviews"],
                verbs=["create"],
            )
        ],
    )
    binding = client.V1ClusterRoleBinding(
        metadata=client.V1ObjectMeta(name=TOKEN_REVIEW_ROLE),
        role_ref=client.V1RoleRef(
            api_group="rbac.authorization.k8s.io",
            kind="ClusterRole",
            name=TOKEN_REVIEW_ROLE,
        ),
        subjects=[client.RbacV1Subject(kind="ServiceAccount", name=NAME, namespace=namespace)],
    )
    _create_or_replace(
        rbac.create_cluster_role,
        lambda body: rbac.replace_cluster_role(TOKEN_REVIEW_ROLE, body),
        role,
    )
    _create_or_replace(
        rbac.create_cluster_role_binding,
        lambda body: rbac.replace_cluster_role_binding(TOKEN_REVIEW_ROLE, body),
        binding,
    )


def apply_script(namespace: str) -> None:
    core = client.CoreV1Api()
    app = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(name=APP_CONFIGMAP),
        data={SERVER_SCRIPT.name: SERVER_SCRIPT.read_text()},
    )
    _create_or_replace(
        lambda body: core.create_namespaced_config_map(namespace, body),
        lambda body: core.replace_namespaced_config_map(APP_CONFIGMAP, namespace, body),
        app,
    )


def apply_configuration(namespace: str, certificates: Certificates) -> None:
    apply_script(namespace)
    core = client.CoreV1Api()
    ca = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(name=CA_CONFIGMAP),
        data={CA_KEY: certificates.ca.decode()},
    )
    tls = client.V1Secret(
        metadata=client.V1ObjectMeta(name=TLS_SECRET),
        type="kubernetes.io/tls",
        string_data={
            "tls.crt": certificates.certificate.decode(),
            "tls.key": certificates.key.decode(),
        },
    )
    _create_or_replace(
        lambda body: core.create_namespaced_config_map(namespace, body),
        lambda body: core.replace_namespaced_config_map(CA_CONFIGMAP, namespace, body),
        ca,
    )
    _create_or_replace(
        lambda body: core.create_namespaced_secret(namespace, body),
        lambda body: core.replace_namespaced_secret(TLS_SECRET, namespace, body),
        tls,
    )


def serving_pod(namespace: str) -> client.V1Pod:
    """The endpoint pod that is ready and not on its way out."""
    pods = client.CoreV1Api().list_namespaced_pod(
        namespace, label_selector=f"app.kubernetes.io/name={NAME}"
    )
    for pod in pods.items:
        if pod.metadata.deletion_timestamp is not None or pod.status.phase != "Running":
            continue
        if all(status.ready for status in pod.status.container_statuses or []):
            return pod
    raise LookupError(f"no ready {NAME} pod in {namespace}")


def endpoint_pod_name(namespace: str) -> str:
    return serving_pod(namespace).metadata.name


def certificates_exist(namespace: str) -> bool:
    core = client.CoreV1Api()
    try:
        core.read_namespaced_secret(TLS_SECRET, namespace)
        core.read_namespaced_config_map(CA_CONFIGMAP, namespace)
    except client.ApiException as exc:
        if exc.status != 404:
            raise
        return False
    return True


def deploy_endpoint(namespace: str, port: int) -> str:
    """Create the endpoint and return its URL once it serves.

    A second deploy keeps the certificates of the first: the running endpoint
    loaded them at startup, and the chart may already mount the CA.
    """
    node_name, host_ip = store_placement(namespace)
    apply_token_review_access(namespace)
    if not certificates_exist(namespace):
        apply_configuration(namespace, generate_certificates([host_ip], namespace))
    else:
        apply_script(namespace)
    apps = client.AppsV1Api()
    _create_or_replace(
        lambda body: apps.create_namespaced_deployment(namespace, body),
        lambda body: apps.replace_namespaced_deployment(NAME, namespace, body),
        endpoint_deployment(namespace, port, node_name),
    )
    deadline = time.monotonic() + READY_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        status = apps.read_namespaced_deployment_status(NAME, namespace).status
        if (status.ready_replicas or 0) >= 1 and (status.updated_replicas or 0) >= 1:
            return credential_url(host_ip, port)
        time.sleep(3)
    raise TimeoutError(f"{NAME} was not ready in {namespace}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("deploy",))
    parser.parse_args(argv)
    namespace = os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", s3.DEFAULT_NAMESPACE)
    config.load_kube_config(os.environ.get("KUBECONFIG"))
    store_port = int(os.environ.get("SNAPSHOT_E2E_S3_PORT", s3.DEFAULT_STORE_PORT))
    url = deploy_endpoint(namespace, port_from_env())
    print("\n".join(helm_set(s3.store_endpoint(namespace, store_port), url)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
