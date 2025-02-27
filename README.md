# progressbar

![CI](https://img.shields.io/badge/CI-passing-green) 
![go report](https://img.shields.io/badge/go%20report-A%2B-green) 
![coverage](https://img.shields.io/badge/coverage-84%25-green) 
![go reference](https://img.shields.io/badge/go-reference-blue)

A very thread-safe bar which should work on every OS without problems. I needed a progressbar for [croc](https://github.com/schollz/croc) and everything I tried had problems, so I made another one. In order to be OS agnostic I do not plan to support multi-line outputs.

## Install

```bash
go get -u github.com/schollz/progressbar/v3
