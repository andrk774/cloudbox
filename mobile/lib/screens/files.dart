import 'dart:io';
import 'dart:typed_data';

import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:open_filex/open_filex.dart';
import 'package:path_provider/path_provider.dart';

import '../api.dart';
import 'settings.dart';

/// Содержимое папки (parent == null — корень).
class FilesScreen extends StatefulWidget {
  const FilesScreen({super.key, required this.api, this.parent, this.title});
  final CloudBoxApi api;
  final String? parent;
  final String? title;

  @override
  State<FilesScreen> createState() => _FilesScreenState();
}

class _FilesScreenState extends State<FilesScreen> {
  Listing? _listing;
  String? _error;
  String _query = '';
  List<Node>? _found;
  final Map<String, double> _uploads = {}; // имя → прогресс

  CloudBoxApi get api => widget.api;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    try {
      final l = await api.list(widget.parent);
      if (mounted) {
        setState(() {
          _listing = l;
          _error = null;
        });
      }
    } on ApiException catch (e) {
      if (mounted) setState(() => _error = e.message);
    }
  }

  void _toast(String m) => ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(m)));

  Future<String?> _ask(String title, String label, [String value = '']) {
    final c = TextEditingController(text: value);
    return showDialog<String>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title),
        content: TextField(controller: c, autofocus: true, decoration: InputDecoration(labelText: label), onSubmitted: (v) => Navigator.pop(ctx, v)),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Отмена')),
          FilledButton(onPressed: () => Navigator.pop(ctx, c.text), child: const Text('OK')),
        ],
      ),
    );
  }

  Future<void> _open(Node n) async {
    if (n.e2e) {
      _toast('Зашифрованные (E2E) папки открываются в веб-версии CloudBox: расшифровка в приложении пока не поддерживается');
      return;
    }
    if (n.isDir) {
      Navigator.of(context).push(MaterialPageRoute(builder: (_) => FilesScreen(api: api, parent: n.id, title: n.name)));
      return;
    }
    try {
      _toast('Скачиваю «${n.name}»…');
      final dir = await getTemporaryDirectory();
      final f = File('${dir.path}/${n.id}-${n.name}');
      final sink = f.openWrite();
      await sink.addStream(await api.download(n.id));
      await sink.close();
      final r = await OpenFilex.open(f.path);
      if (r.type != ResultType.done && mounted) _toast('Нет приложения, чтобы открыть этот файл');
    } on ApiException catch (e) {
      _toast(e.message);
    }
  }

  Future<void> _upload() async {
    final res = await FilePicker.pickFiles(); // пустой список — пользователь отменил выбор
    for (final pf in res) {
      if (pf.path == null) continue;
      final file = File(pf.path!);
      setState(() => _uploads[pf.name] = 0);
      final raf = await file.open();
      try {
        await api.upload(
          parent: widget.parent,
          name: pf.name,
          size: await raf.length(),
          read: (off, len) async {
            await raf.setPosition(off);
            return Uint8List.fromList(await raf.read(len));
          },
          onProgress: (p) => mounted ? setState(() => _uploads[pf.name] = p) : null,
        );
      } on ApiException catch (e) {
        _toast('«${pf.name}»: ${e.message}');
      } finally {
        await raf.close();
        if (mounted) setState(() => _uploads.remove(pf.name));
      }
    }
    _load();
  }

  Future<void> _newFolder() async {
    final name = (await _ask('Новая папка', 'Название'))?.trim();
    if (name == null || name.isEmpty) return;
    try {
      await api.mkdir(widget.parent, name);
      _load();
    } on ApiException catch (e) {
      _toast(e.message);
    }
  }

  void _menu(Node n) {
    showModalBottomSheet<void>(
      context: context,
      builder: (ctx) => SafeArea(
        child: Wrap(children: [
          ListTile(title: Text(n.name, style: const TextStyle(fontWeight: FontWeight.w600))),
          if (!n.e2e) ListTile(leading: const Icon(Icons.open_in_new), title: const Text('Открыть'), onTap: () {
            Navigator.pop(ctx);
            _open(n);
          }),
          ListTile(
            leading: const Icon(Icons.edit),
            title: const Text('Переименовать'),
            onTap: () async {
              Navigator.pop(ctx);
              final name = (await _ask('Переименовать', 'Новое имя', n.name))?.trim();
              if (name == null || name.isEmpty || name == n.name) return;
              try {
                await api.rename(n.id, name);
                _load();
              } on ApiException catch (e) {
                _toast(e.message);
              }
            },
          ),
          ListTile(
            leading: const Icon(Icons.delete_outline),
            title: const Text('Удалить'),
            onTap: () async {
              Navigator.pop(ctx);
              try {
                await api.trash(n.id);
                _toast('«${n.name}» — в корзине');
                _load();
              } on ApiException catch (e) {
                _toast(e.message);
              }
            },
          ),
        ]),
      ),
    );
  }

  Widget _leading(Node n) {
    if (n.isDir) return Icon(n.e2e ? Icons.lock : Icons.folder, color: n.e2e ? Colors.teal : Colors.amber.shade700, size: 36);
    if (n.hasThumb) {
      return ClipRRect(
        borderRadius: BorderRadius.circular(6),
        child: Image.network(api.thumbUrl(n.id, 128).toString(), headers: api.authHeaders, width: 40, height: 40, fit: BoxFit.cover,
            errorBuilder: (_, _, _) => const Icon(Icons.image, size: 36)),
      );
    }
    return const Icon(Icons.insert_drive_file_outlined, size: 36);
  }

  Future<void> _search() async {
    final q = (await _ask('Поиск', 'Имя или текст внутри файла', _query))?.trim();
    if (q == null) return;
    if (q.isEmpty) {
      setState(() {
        _query = '';
        _found = null;
      });
      return;
    }
    try {
      final r = await api.search(q);
      setState(() {
        _query = q;
        _found = r;
      });
    } on ApiException catch (e) {
      _toast(e.message);
    }
  }

  @override
  Widget build(BuildContext context) {
    final items = _found ?? _listing?.items;
    final canWrite = _listing?.canWrite ?? true;
    return Scaffold(
      appBar: AppBar(
        title: Text(_found != null ? 'Поиск: $_query' : (widget.title ?? 'Все файлы')),
        actions: [
          IconButton(icon: Icon(_found != null ? Icons.close : Icons.search), tooltip: 'Поиск',
              onPressed: _found != null
                  ? () => setState(() {
                        _found = null;
                        _query = '';
                      })
                  : _search),
          if (widget.parent == null)
            IconButton(icon: const Icon(Icons.settings), tooltip: 'Настройки',
                onPressed: () => Navigator.of(context).push(MaterialPageRoute(builder: (_) => SettingsScreen(api: api)))),
        ],
      ),
      floatingActionButton: canWrite && _found == null
          ? FloatingActionButton(
              key: const Key('add'),
              onPressed: () => showModalBottomSheet<void>(
                context: context,
                builder: (ctx) => SafeArea(
                  child: Wrap(children: [
                    ListTile(leading: const Icon(Icons.upload_file), title: const Text('Загрузить файлы'), onTap: () {
                      Navigator.pop(ctx);
                      _upload();
                    }),
                    ListTile(leading: const Icon(Icons.create_new_folder), title: const Text('Новая папка'), onTap: () {
                      Navigator.pop(ctx);
                      _newFolder();
                    }),
                  ]),
                ),
              ),
              child: const Icon(Icons.add),
            )
          : null,
      body: _error != null && items == null
          ? Center(child: Padding(padding: const EdgeInsets.all(24), child: Text(_error!, textAlign: TextAlign.center)))
          : items == null
              ? const Center(child: CircularProgressIndicator())
              : RefreshIndicator(
                  onRefresh: _load,
                  child: ListView(children: [
                    for (final e in _uploads.entries)
                      ListTile(leading: const Icon(Icons.cloud_upload), title: Text(e.key), subtitle: LinearProgressIndicator(value: e.value)),
                    if (items.isEmpty) Padding(padding: const EdgeInsets.all(48), child: Text(_found != null ? 'Ничего не найдено' : 'Папка пуста', textAlign: TextAlign.center)),
                    for (final n in items)
                      ListTile(
                        leading: _leading(n),
                        title: Text(n.name, maxLines: 1, overflow: TextOverflow.ellipsis),
                        subtitle: Text(n.e2e ? 'зашифрованная папка — откройте в веб-версии' : n.isDir ? 'папка' : '${formatSize(n.size)} · ${_date(n.updatedAt)}'),
                        onTap: () => _open(n),
                        onLongPress: canWrite ? () => _menu(n) : null,
                        trailing: canWrite ? IconButton(icon: const Icon(Icons.more_vert), onPressed: () => _menu(n)) : null,
                      ),
                  ]),
                ),
    );
  }
}

String _date(DateTime d) {
  final l = d.toLocal();
  String two(int v) => v.toString().padLeft(2, '0');
  return '${two(l.day)}.${two(l.month)}.${l.year} ${two(l.hour)}:${two(l.minute)}';
}
