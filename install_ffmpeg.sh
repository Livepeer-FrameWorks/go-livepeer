#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/d3d6dd28edc12b4d0fee03f1bd1d4036c59e7e89/install_ffmpeg.sh | bash -s $1
