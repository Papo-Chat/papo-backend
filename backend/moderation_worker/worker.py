#!/usr/bin/env python3
"""Worker de moderação de imagens do Papo.

Processo persistente supervisionado pelo backend Go: recebe requisições por
socket Unix (NDJSON: um objeto JSON por linha), classifica imagens com um
modelo ONNX local e devolve as probabilidades por classe.

Protocolo:
  classify: {"request_id": "...", "path": "/abs/path/img.jpg", "mime": "image/jpeg"}
    -> {"request_id": "...", "sfw": 0.99, "nudity": 0.001, "gore": 0.002,
         "model": "safety-xs-v1"}
  health:   {"type": "health"}
    -> {"type": "health", "status": "ok", "model": "safety-xs-v1"}
  erro:     {"request_id": "...", "error": "..."}

Ordem das classes do modelo (OwenElliott/image-safety-classifier-xs):
[NSFL, NSFW, SFW] -> gore, nudity, sfw. O grafo ONNX já inclui normalização e
softmax (entrada: 224x224 RGB, pixels 0-255).

Limites de segurança:
  - O worker só lê arquivos dentro de --media-dir (containment de path com
    realpath: symlinks que apontam para fora são rejeitados).
  - Não há execução de código arbitrário: o parser de entrada é json.loads
    (dados, não código), o modelo ONNX é fixo (pinado por SHA-256 no Go e
    carregado uma única vez no boot) e o PIL/onnxruntime têm versões
    pinadas em requirements.txt.
"""
import argparse
import json
import os
import socket

import numpy as np
import onnxruntime as ort
from PIL import Image

INPUT_SIZE = 224
MAX_REQUEST_BYTES = 64 * 1024
MAX_PATH_BYTES = 4096
# Mesmo limite do pipeline de imagens (THUMBNAIL_MAX_PIXELS): evita
# decompression bomb em memória.
MAX_PIXELS = 50_000_000
# Máximo de frames amostrados de uma imagem animada (GIF/WebP): 0, 25%, 50%,
# 75% e o último. Evita o bypass de "frame 0 inocente, resto NSFW".
MAX_FRAMES = 5


def parse_args():
    parser = argparse.ArgumentParser(description="Papo image moderation worker")
    parser.add_argument("--safety-model", required=True, help="caminho do modelo ONNX de safety")
    parser.add_argument("--socket", required=True, dest="socket_path", help="caminho do socket Unix")
    parser.add_argument("--media-dir", required=True, help="pasta de mídia (única pasta legível pelo worker)")
    return parser.parse_args()


def load_session(model_path):
    options = ort.SessionOptions()
    options.log_severity_level = 3  # somente erros
    return ort.InferenceSession(model_path, sess_options=options)


def resolve_media_path(path, media_dir):
    # Containment: o caminho (com symlinks resolvidos) precisa estar dentro
    # de media_dir. realpath impede escape via symlink para fora da pasta.
    real = os.path.realpath(path)
    base = os.path.realpath(media_dir)
    if os.path.commonpath([base, real]) != base:
        raise ValueError("caminho fora da pasta de mídia")
    return real


def load_frames(image_path):
    """Retorna os frames da imagem prontos para inferência.

    Estática: 1 frame. Animada (GIF/WebP): amostragem de até MAX_FRAMES
    posições (0, 25%, 50%, 75% e último) — o classificador vê mais que o
    primeiro frame, fechando o bypass de animação com conteúdo no meio/fim.
    """
    with Image.open(image_path) as img:
        width, height = img.size
        if width * height > MAX_PIXELS:
            raise ValueError("imagem excede o limite de pixels")
        total = getattr(img, "n_frames", 1)
        if total <= MAX_FRAMES:
            indices = list(range(total))
        else:
            indices = sorted(
                {0, round(total * 0.25), round(total * 0.50), round(total * 0.75), total - 1}
            )
        frames = []
        for index in indices:
            if index:
                img.seek(index)
            # convert() materializa o frame atual (o with fecha o arquivo
            # antes da inferência, então o dado precisa sair do lazy load).
            frames.append(img.convert("RGB").resize((INPUT_SIZE, INPUT_SIZE), Image.Resampling.BILINEAR))
    return frames


def classify_frame(session, frame):
    pixels = np.asarray(frame, dtype=np.float32).transpose(2, 0, 1)[np.newaxis]
    input_name = session.get_inputs()[0].name
    probs = session.run(None, {input_name: pixels})[0][0]
    nsfl, nsfw, sfw = (float(p) for p in probs[:3])
    return {"gore": nsfl, "nudity": nsfw, "sfw": sfw}


def classify(session, image_path):
    # Imagem animada: classifica cada frame amostrado e usa o maior score de
    # cada categoria (o frame mais arriscado define o resultado).
    worst = None
    for frame in load_frames(image_path):
        scores = classify_frame(session, frame)
        if worst is None:
            worst = scores
        else:
            for key in worst:
                if scores[key] > worst[key]:
                    worst[key] = scores[key]
    return worst


def read_request(conn):
    buf = bytearray()
    while not buf.endswith(b"\n"):
        chunk = conn.recv(65536)
        if not chunk:
            return None
        buf.extend(chunk)
        if len(buf) > MAX_REQUEST_BYTES:
            raise ValueError("requisição excede o limite")
    line = bytes(buf).rstrip(b"\r\n")
    if not line:
        return None
    return json.loads(line.decode("utf-8"))


def handle(conn, session, model_name, media_dir):
    try:
        request = read_request(conn)
    except (ValueError, json.JSONDecodeError) as exc:
        conn.sendall((json.dumps({"error": f"bad request: {exc}"}) + "\n").encode("utf-8"))
        return
    if request is None:
        return

    if request.get("type") == "health":
        response = {"type": "health", "status": "ok", "model": model_name}
    else:
        request_id = str(request.get("request_id", ""))[:128]
        path = request.get("path")
        if not isinstance(path, str) or not path or len(path) > MAX_PATH_BYTES:
            response = {"request_id": request_id, "error": "invalid path"}
        else:
            try:
                safe_path = resolve_media_path(path, media_dir)
                response = {"request_id": request_id, "model": model_name, **classify(session, safe_path)}
            except Exception as exc:
                response = {"request_id": request_id, "error": str(exc)[:512]}
    conn.sendall((json.dumps(response) + "\n").encode("utf-8"))


def serve(socket_path, session, model_name, media_dir):
    if os.path.exists(socket_path):
        os.unlink(socket_path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(socket_path)
    # Protege o pathname do socket (somente o usuário do serviço conecta).
    os.chmod(socket_path, 0o600)
    server.listen(16)
    print(f"moderation worker ready (model={model_name}, socket={socket_path}, media_dir={media_dir})", flush=True)
    while True:
        conn, _ = server.accept()
        try:
            handle(conn, session, model_name, media_dir)
        except OSError:
            pass
        finally:
            conn.close()


def main():
    args = parse_args()
    model_name = os.path.basename(args.safety_model)
    session = load_session(args.safety_model)
    serve(args.socket_path, session, model_name, args.media_dir)


if __name__ == "__main__":
    main()
