# Starbound Mod Manager

<p align="center">
  <img src="resources/icon.png" alt="Starbound Mod Manager" width="112">
</p>

<p align="center">
  <a href="#english">English</a> · <a href="#简体中文">简体中文</a>
</p>

## English

Starbound Mod Manager is a native desktop app built with Go and [MyGo](https://github.com/egoist/mygo). It scans local mods and Steam Workshop content, previews asset conflicts, manages load order, and builds merged `.pak` files in the game's local `mods` directory.

### Features

- Scan `.pak` files, unpacked mod folders, and symbolic links.
- Preview duplicate asset paths and see which mod wins in the current order.
- Arrange mods and apply the order by writing `priority` metadata to ordinary mods.
- Select mods, edit the output filename and metadata, then build and install an `SBAsset6` `.pak`.
- Manage a merged package like any other mod: reorder it, enable or disable it, or include it in a later merge.
- Enable or disable individual mods. A merge disables its selected source mods; enable any source again from the list when needed.
- Optionally launch Starbound from the manager, directly or through Steam.
- Switch the interface between Simplified Chinese and English.

### Load order

The list runs from top to bottom in the intended load order. Applied priorities for ordinary mods start at `0` at the top and increase down the list. The Workshop translation items [807695810](https://steamcommunity.com/sharedfiles/filedetails/?id=807695810) and [2240879531](https://steamcommunity.com/sharedfiles/filedetails/?id=2240879531) stay pinned at the top and keep the priorities in their files. The base `assets/packed.pak` uses priority `-9999`. Starbound's `requires` and `includes` rules may still affect the final load order.

### Usage

Scanning is shared setup; applying an order, building a merged mod, and launching the game are separate actions. Launching from the manager is optional.

#### Apply a load order (optional)

Use the arrow buttons to arrange the list. **Apply order** writes priorities to ordinary mod metadata. This does not build a merged mod.

#### Build a merged mod (optional)

Select the source mods and choose **Build & install**. Applying the load order first is not required. The current list order determines which selected mod wins duplicate assets inside the merged package; building does not write priorities to the source mods. The merged package priority is set to the highest current priority among the selected mods. The selected source mods are disabled after installation; use **Enable** in the list to re-enable any mod individually.

#### Launch from the manager (optional)

Choose **Direct launch** or **Launch via Steam** if you want to start the game here. Direct launch does not load Workshop mods; Steam launch loads subscribed items. You can also close the manager and launch the game in your usual way.

### Notes

Applying an order changes mod metadata. Disabled `.pak` files use the `.pak.bak` suffix; disabled unpacked folders receive the `_smm-disabled_` prefix. The manager can still scan and read disabled mods. Steam updates may reset changes to Workshop files or restore renamed source files.

### Build

Go 1.27.3 or later is required. The interface uses MyGo's native UI and does not need Bun, Node.js, or a JavaScript frontend toolchain.

```sh
go run .
go build -o Starbound-ModManager .
```

To cross-build a Windows x64 desktop app with the [MyGo CLI](https://github.com/egoist/mygo):

```sh
mygo build -platform windows/amd64 -skip-installer -o dist
```

### Settings

The app saves `Starbound-ModManager.json` in its current working directory.

### Release

Create and publish a GitHub Release manually. When it is published, the `Build and upload release` workflow builds a Windows x64 `.exe`, Linux x64 `.tar.gz` and `.deb` packages, and a universal macOS `.dmg`, then uploads them to that release.

## 简体中文

Starbound Mod Manager 是使用 Go 和 [MyGo](https://github.com/egoist/mygo) 原生 UI 构建的桌面工具。它可以扫描本地模组和 Steam Workshop 内容、预览资源冲突、管理加载顺序，并在游戏本地 `mods` 目录中生成 `.pak` 整合包。

### 功能

- 扫描 `.pak` 文件、解包模组目录和符号链接。
- 预览重复资源路径，并查看当前顺序下由哪个模组覆盖。
- 调整模组顺序；点击“应用排序”后，将 `priority` 写入普通模组的 metadata。
- 选择模组、编辑输出文件名和 metadata，然后生成并安装 `SBAsset6` `.pak`。
- 整合包按普通 mod 管理：可调整顺序、启用或停用，也可继续作为后续整合的输入。
- 可单独启用或停用模组。整合时会停用选中的输入模组；需要时可在列表中重新启用。
- 可选：从管理器直接启动 Starbound，或经 Steam 启动。
- 界面支持简体中文和英语。

### 加载顺序

列表从上到下表示预期加载顺序。应用排序后，普通模组的 priority 从顶部的 `0` 开始，越往下数值越大。Workshop 本体汉化项 [807695810](https://steamcommunity.com/sharedfiles/filedetails/?id=807695810) 和 [2240879531](https://steamcommunity.com/sharedfiles/filedetails/?id=2240879531) 固定在列表顶部，并保留各自文件中的 priority。基础资源 `assets/packed.pak` 的 priority 为 `-9999`。Starbound 的 `requires` 和 `includes` 规则仍可能影响最终加载顺序。

### 使用

先选择并扫描 Starbound 安装目录或模组来源目录。应用排序、生成整合包和启动游戏是彼此独立的操作；从管理器启动游戏是可选项。

#### 调整加载顺序（可选）

使用箭头调整列表顺序，点击“应用排序”后才会将 priority 写入普通模组的 metadata。此操作不会生成整合包。

#### 生成整合包（可选）

勾选要整合的模组，点击“生成并安装整合包”，然后在弹窗中填写输出文件名和 metadata。无需先应用排序；当前列表顺序决定整合包内重复资源由哪个输入模组提供，生成时不会改写输入模组的 priority。整合包使用所选输入模组当前 priority 中的最大值。安装后，所选输入模组会停用；需要时可在列表中逐个点击“启用”。

#### 从管理器启动游戏（可选）

需要从管理器启动时，选择“直接启动”或“经 Steam 启动”。直接启动不会加载 Workshop 模组；经 Steam 启动会加载已订阅的项目。也可以关闭管理器，照常从其他入口启动游戏。

### 说明

应用排序会修改模组 metadata。停用的 `.pak` 会使用 `.pak.bak` 后缀；停用的解包目录会添加 `_smm-disabled_` 前缀。管理器仍可扫描和读取已停用的模组。Steam 更新可能重置 Workshop 文件的修改，或恢复被改名的源文件。

### 构建

需要 Go 1.27.3 或更新版本。界面使用 MyGo 原生 UI，不需要 Bun、Node.js 或 JavaScript 前端构建工具。

```sh
go run .
go build -o Starbound-ModManager .
```

使用 [MyGo CLI](https://github.com/egoist/mygo) 交叉构建 Windows x64 桌面程序：

```sh
mygo build -platform windows/amd64 -skip-installer -o dist
```

### 配置

程序将 `Starbound-ModManager.json` 保存在启动时的当前工作目录。

### 发布版本

手动创建并发布 GitHub Release。发布后，`Build and upload release` workflow 会自动构建 Windows x64 `.exe`、Linux x64 `.tar.gz` 和 `.deb`，以及 macOS 通用版 `.dmg`，并上传到该 Release。
