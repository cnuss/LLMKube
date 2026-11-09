# Real hailo-ollama server built against HailoRT's public headers and a fake
# libhailort that emulates a Hailo-10H (AI HAT+ 2). See fake_hailort.cpp.
FROM debian:trixie-slim AS build

ARG HAILORT_REF=e7727f7482477e448f4c93b33dc62f9e5a30c3e7
ARG HAILO_GENAI_REF=e4ab0ff8d9fb08735f1c71ab80ba691ed5b38a91

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates cmake g++ git libssl-dev make pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git init -q hailort && cd hailort \
    && git remote add origin https://github.com/hailo-ai/hailort.git \
    && git fetch -q --depth 1 --filter=blob:none origin "$HAILORT_REF" \
    && git sparse-checkout set hailort/libhailort/include \
    && git checkout -q FETCH_HEAD
RUN git init -q genai && cd genai \
    && git remote add origin https://github.com/hailo-ai/hailo_model_zoo_genai.git \
    && git fetch -q --depth 1 origin "$HAILO_GENAI_REF" \
    && git checkout -q FETCH_HEAD

COPY fake_hailort.cpp cmake/ /src/sim/
RUN mkdir -p /opt/hailort/lib /opt/hailort/lib/cmake/HailoRT \
    && cp -r hailort/hailort/libhailort/include /opt/hailort/include \
    && g++ -std=c++17 -O2 -fPIC -shared -fvisibility=hidden -Wl,-soname,libhailort.so.5.5.0 \
        -I/opt/hailort/include -o /opt/hailort/lib/libhailort.so.5.5.0 sim/fake_hailort.cpp \
    && ln -s libhailort.so.5.5.0 /opt/hailort/lib/libhailort.so \
    && cp sim/HailoRTConfig.cmake sim/HailoRTConfigVersion.cmake /opt/hailort/lib/cmake/HailoRT/

RUN cmake -S genai -B genai/build -DCMAKE_BUILD_TYPE=Release -DCMAKE_PREFIX_PATH=/opt/hailort \
    && cmake --build genai/build -j"$(nproc)" \
    && DESTDIR=/out cmake --install genai/build --prefix /usr/local \
    && sed -n 's/^project(hailo-ollama VERSION \([0-9.]*\)).*/\1/p' genai/CMakeLists.txt >/src/version

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3t64 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/usr/local/ /usr/local/
COPY --from=build /opt/hailort/lib/libhailort.so.5.5.0 /usr/local/lib/
COPY --from=build /src/version /usr/local/share/hailo-sim/version
COPY entrypoint.sh /usr/local/bin/hailo-sim-entrypoint
RUN ldconfig
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/hailo-sim-entrypoint"]
