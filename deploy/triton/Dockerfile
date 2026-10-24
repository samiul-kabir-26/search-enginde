# Triton with the Python-backend dependencies used by the preprocessing models
# of the clip_image, clip_text and bge_text ensembles (docs/MODELS.md §3).
FROM nvcr.io/nvidia/tritonserver:26.09-py3
RUN pip install --no-cache-dir \
      transformers==5.20.2 \
      tokenizers==0.23.3 \
      pillow==13.0.0 \
      numpy==2.5.4
