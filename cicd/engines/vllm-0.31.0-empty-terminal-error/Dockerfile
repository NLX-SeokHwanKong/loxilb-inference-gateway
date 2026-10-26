FROM vllm/vllm-openai@sha256:a4a4c0437bf7240089da5f08aa370c4aee17ae5290f7a3b468825ee26c4c3a6b
COPY SOURCE.json apply_patch.py /opt/vllm-upstream-40986/
RUN python3 /opt/vllm-upstream-40986/apply_patch.py
LABEL io.loxilb.engine-patch="vllm-0.31.0-upstream-40986"
